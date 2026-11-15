package sigv4

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// The x-amz-content-sha256 values that stand for something other than the
// SHA-256 of the body.
const (
	UnsignedPayload                 = "UNSIGNED-PAYLOAD"
	StreamingSignedPayload          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingSignedPayloadTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsignedPayloadTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	streamingV4APrefix              = "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// payloadKind is how a request's body is covered by its signature.
type payloadKind int

const (
	payloadUnsigned payloadKind = iota
	payloadHashed
	payloadStreaming
	payloadStreamingTrailer
)

// classifyContentHash reads an x-amz-content-sha256 value.
func classifyContentHash(value string) (payloadKind, *Failure) {
	switch {
	case value == UnsignedPayload, value == StreamingUnsignedPayloadTrailer:
		return payloadUnsigned, nil
	case value == StreamingSignedPayload:
		return payloadStreaming, nil
	case value == StreamingSignedPayloadTrailer:
		return payloadStreamingTrailer, nil
	case strings.HasPrefix(value, streamingV4APrefix):
		return 0, fail(KindUnsupportedAlgorithm, "SigV4a streaming payloads ("+value+") are not supported in strict signature mode")
	case sha256Hex.MatchString(value):
		return payloadHashed, nil
	}
	return 0, fail(KindInvalidContentHash, "x-amz-content-sha256 must be UNSIGNED-PAYLOAD, STREAMING-UNSIGNED-PAYLOAD-TRAILER, STREAMING-AWS4-HMAC-SHA256-PAYLOAD, STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER or a valid sha256 value.")
}

// memorySpoolLimit is how much of a body is held in memory before the
// rest goes to a temporary file.
const memorySpoolLimit = 8 << 20

// spool keeps a copy of a body so it can be replayed to the handler after
// verification has read it.
type spool struct {
	mem  bytes.Buffer
	file *os.File
	err  error
}

func (s *spool) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.file == nil && s.mem.Len()+len(p) <= memorySpoolLimit {
		return s.mem.Write(p)
	}
	if s.file == nil {
		f, err := os.CreateTemp("", "stratus-sigv4-*")
		if err != nil {
			s.err = err
			return 0, err
		}
		s.file = f
	}
	n, err := s.file.Write(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// replay returns the spooled bytes; closing it removes any temporary file.
func (s *spool) replay() (io.ReadCloser, error) {
	if s.err != nil {
		s.discard()
		return nil, s.err
	}
	if s.file == nil {
		return io.NopCloser(bytes.NewReader(s.mem.Bytes())), nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		s.discard()
		return nil, err
	}
	return &spooledFile{head: bytes.NewReader(s.mem.Bytes()), file: s.file}, nil
}

func (s *spool) discard() {
	if s.file != nil {
		name := s.file.Name()
		_ = s.file.Close()
		_ = os.Remove(name)
	}
}

type spooledFile struct {
	head *bytes.Reader
	file *os.File
}

func (f *spooledFile) Read(p []byte) (int, error) {
	if f.head.Len() > 0 {
		return f.head.Read(p)
	}
	return f.file.Read(p)
}

func (f *spooledFile) Close() error {
	name := f.file.Name()
	err := f.file.Close()
	_ = os.Remove(name)
	return err
}

// errLineTooLong is a chunk header or trailer line past maxChunkLine.
var errLineTooLong = errors.New("aws-chunked line too long")

// chunkSigner verifies the signature chain of an aws-chunked body.
type chunkSigner struct {
	key      []byte
	amzDate  string
	scope    string
	previous string
	trailer  bool
}

const (
	chunkAlgorithm   = "AWS4-HMAC-SHA256-PAYLOAD"
	trailerAlgorithm = "AWS4-HMAC-SHA256-TRAILER"
	maxChunkLine     = 4096
)

// verify reads the whole aws-chunked body from src and checks every chunk
// signature (and the trailer signature) against the chain seeded by the
// request signature. It returns the decoded length.
func (c *chunkSigner) verify(src *bufio.Reader) (int64, *Failure, error) {
	var decoded int64
	for n := 1; ; n++ {
		line, err := readLine(src)
		switch {
		case errors.Is(err, io.EOF):
			return decoded, malformedChunk("the body ended before the final zero-length chunk"), nil
		case errors.Is(err, errLineTooLong):
			return decoded, malformedChunk(fmt.Sprintf("the header of chunk %d is too long", n)), nil
		case err != nil:
			return decoded, nil, err
		}
		size, signature, f := parseChunkHeader(line)
		if f != nil {
			return decoded, f, nil
		}
		h := sha256.New()
		if _, err := io.CopyN(h, src, size); err != nil {
			if errors.Is(err, io.EOF) {
				return decoded, malformedChunk(fmt.Sprintf("the body ended inside chunk %d", n)), nil
			}
			return decoded, nil, err
		}
		decoded += size
		if size > 0 {
			if f, err := expectCRLF(src, n); f != nil || err != nil {
				return decoded, f, err
			}
		}
		stringToSign := strings.Join([]string{chunkAlgorithm, c.amzDate, c.scope, c.previous, EmptyPayloadHash, hex.EncodeToString(h.Sum(nil))}, "\n")
		if f := c.check(signature, stringToSign, fmt.Sprintf("chunk %d", n)); f != nil {
			return decoded, f, nil
		}
		if size == 0 {
			break
		}
	}
	if c.trailer {
		f, err := c.verifyTrailer(src)
		return decoded, f, err
	}
	// The final zero-length chunk is followed by an empty line.
	if _, err := readLine(src); err != nil && !errors.Is(err, io.EOF) {
		return decoded, nil, err
	}
	return decoded, nil, nil
}

func (c *chunkSigner) verifyTrailer(src *bufio.Reader) (*Failure, error) {
	var canonical strings.Builder
	for {
		line, err := readLine(src)
		switch {
		case errors.Is(err, io.EOF):
			return malformedChunk("the body ended before x-amz-trailer-signature"), nil
		case errors.Is(err, errLineTooLong):
			return malformedChunk("a trailer line is too long"), nil
		case err != nil:
			return nil, err
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return malformedChunk("trailer line " + strconv.Quote(line) + " is not name:value"), nil
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "x-amz-trailer-signature" {
			stringToSign := strings.Join([]string{trailerAlgorithm, c.amzDate, c.scope, c.previous, hashHex([]byte(canonical.String()))}, "\n")
			if f := c.check(strings.TrimSpace(value), stringToSign, "trailer"); f != nil {
				return f, nil
			}
			if _, err := readLine(src); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return nil, nil
		}
		canonical.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
}

func (c *chunkSigner) check(provided, stringToSign, what string) *Failure {
	expected := Signature(c.key, stringToSign)
	if !equalSignatures(provided, expected) {
		f := fail(KindSignatureMismatch, "The "+what+" signature we calculated does not match the signature you provided.")
		f.SignatureProvided = provided
		f.StringToSign = stringToSign
		return f
	}
	c.previous = expected
	return nil
}

func parseChunkHeader(line string) (int64, string, *Failure) {
	sizeHex, ext, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 {
		return 0, "", malformedChunk("chunk size " + strconv.Quote(sizeHex) + " is not hexadecimal")
	}
	name, signature, ok := strings.Cut(ext, "=")
	if !ok || strings.TrimSpace(name) != "chunk-signature" || signature == "" {
		return 0, "", malformedChunk("chunk header " + strconv.Quote(line) + " has no chunk-signature")
	}
	return size, strings.TrimSpace(signature), nil
}

func expectCRLF(src *bufio.Reader, n int) (*Failure, error) {
	var crlf [2]byte
	if _, err := io.ReadFull(src, crlf[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return malformedChunk(fmt.Sprintf("chunk %d is not followed by CRLF", n)), nil
		}
		return nil, err
	}
	if crlf != [2]byte{'\r', '\n'} {
		return malformedChunk(fmt.Sprintf("chunk %d is longer than its declared size", n)), nil
	}
	return nil, nil
}

// readLine reads one CRLF-terminated line without the terminator.
func readLine(src *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		part, isPrefix, err := src.ReadLine()
		if err != nil {
			return "", err
		}
		b.Write(part)
		if b.Len() > maxChunkLine {
			return "", errLineTooLong
		}
		if !isPrefix {
			return b.String(), nil
		}
	}
}

func malformedChunk(message string) *Failure {
	return fail(KindMalformedChunk, "The aws-chunked body is malformed: "+message+".")
}

// hashingCopy copies src to dst and returns its SHA-256.
func hashingCopy(dst io.Writer, src io.Reader) (hash.Hash, error) {
	h := sha256.New()
	_, err := io.Copy(io.MultiWriter(dst, h), src)
	return h, err
}
