package ses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	identitiesBucket = "ses-identities"
	templatesBucket  = "ses-templates"
	messagesBucket   = "ses-messages"

	region    = "us-east-1"
	accountID = "000000000000"

	// Quotas reported by GetSendQuota and GetAccount. They match the SES
	// sandbox so code that checks them sees familiar numbers; nothing is
	// enforced.
	max24HourSend = 200.0
	maxSendRate   = 1.0
)

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	Now      func() time.Time
}

// Service holds identities, templates and the inbox. It is protocol
// agnostic; query.go and v2.go adapt it to the two SES API families.
type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
	seq      uint64
}

// NewService builds an SES service.
func NewService(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{metadata: opts.Metadata, now: now}
}

// VerifyIdentity registers an address or domain as a verified sender. It is
// idempotent, like the SES call.
func (s *Service) VerifyIdentity(name string) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return Identity{}, invalidParameter("Identity is required")
	}
	if existing, ok, err := s.loadIdentity(name); err != nil {
		return Identity{}, err
	} else if ok {
		return existing, nil
	}
	identity := Identity{Name: name, Type: IdentityTypeFor(name), VerifiedAt: s.now().UTC()}
	if identity.Type == IdentityDomain {
		identity.VerificationToken = randomToken()
	}
	if err := s.put(identitiesBucket, identityKey(name), identity); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

// Identity returns a verified identity by name.
func (s *Service) Identity(name string) (Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadIdentity(name)
}

// Identities lists verified identities, optionally of one type, sorted by
// name.
func (s *Service) Identities(identityType string) ([]Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Identity, 0)
	err := s.metadata.Scan(identitiesBucket, "", func(_, v []byte) error {
		var identity Identity
		if json.Unmarshal(v, &identity) == nil && (identityType == "" || identity.Type == identityType) {
			items = append(items, identity)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// DeleteIdentity forgets an identity. Deleting an unknown one is not an
// error, as in SES.
func (s *Service) DeleteIdentity(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.metadata.Delete(identitiesBucket, identityKey(name)); err != nil {
		return internal(err)
	}
	return nil
}

// CreateTemplate stores a template. The name must be new.
func (s *Service) CreateTemplate(t Template) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateTemplate(t); err != nil {
		return Template{}, err
	}
	if _, ok, err := s.loadTemplate(t.Name); err != nil {
		return Template{}, err
	} else if ok {
		return Template{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "AlreadyExists", Message: "template already exists: " + t.Name}
	}
	t.CreatedAt = s.now().UTC()
	if err := s.put(templatesBucket, t.Name, t); err != nil {
		return Template{}, err
	}
	return t, nil
}

// UpdateTemplate replaces an existing template's content.
func (s *Service) UpdateTemplate(t Template) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateTemplate(t); err != nil {
		return Template{}, err
	}
	existing, ok, err := s.loadTemplate(t.Name)
	if err != nil {
		return Template{}, err
	}
	if !ok {
		return Template{}, templateNotFound(t.Name)
	}
	t.CreatedAt = existing.CreatedAt
	if err := s.put(templatesBucket, t.Name, t); err != nil {
		return Template{}, err
	}
	return t, nil
}

// Template returns a template by name.
func (s *Service) Template(name string) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok, err := s.loadTemplate(name)
	if err != nil {
		return Template{}, err
	}
	if !ok {
		return Template{}, templateNotFound(name)
	}
	return t, nil
}

// Templates lists templates sorted by name.
func (s *Service) Templates() ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Template, 0)
	err := s.metadata.Scan(templatesBucket, "", func(_, v []byte) error {
		var t Template
		if json.Unmarshal(v, &t) == nil {
			items = append(items, t)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// DeleteTemplate forgets a template. Deleting an unknown one is not an
// error, as in SES.
func (s *Service) DeleteTemplate(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.metadata.Delete(templatesBucket, name); err != nil {
		return internal(err)
	}
	return nil
}

// Send validates an email the way the SES sandbox does (the sender must be
// a verified identity) and captures it in the inbox instead of delivering
// it. It returns the stored message, whose ID is the MessageId.
func (s *Service) Send(in SendInput) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := Message{
		Source:           strings.TrimSpace(in.Source),
		Destination:      in.Destination,
		ReplyTo:          in.ReplyTo,
		Subject:          in.Subject,
		Tags:             in.Tags,
		ConfigurationSet: in.ConfigurationSet,
		API:              in.API,
	}
	switch {
	case len(in.Raw) > 0:
		from, dest, subject, body := parseRaw(in.Raw)
		if msg.Source == "" {
			msg.Source = from
		}
		if len(msg.Destination.Recipients()) == 0 {
			msg.Destination = dest
		}
		msg.Subject, msg.Body, msg.Raw = subject, body, string(in.Raw)
	case in.Template != "":
		t, ok, err := s.loadTemplate(in.Template)
		if err != nil {
			return Message{}, err
		}
		if !ok {
			return Message{}, templateNotFound(in.Template)
		}
		subject, body, err := t.Render(in.TemplateData)
		if err != nil {
			return Message{}, err
		}
		msg.Subject, msg.Body = subject, body
		msg.Template = &TemplateRef{Name: in.Template, Data: in.TemplateData}
	case in.Simple != nil:
		if in.Subject == "" {
			return Message{}, invalidParameter("Message.Subject is required")
		}
		if in.Simple.Text == "" && in.Simple.HTML == "" {
			return Message{}, invalidParameter("Message.Body must have a Text or Html part")
		}
		msg.Body = *in.Simple
	default:
		return Message{}, invalidParameter("Message, Template or RawMessage is required")
	}

	if msg.Source == "" {
		return Message{}, invalidParameter("Source is required")
	}
	if len(msg.Destination.Recipients()) == 0 {
		return Message{}, invalidParameter("Destination must have at least one recipient")
	}
	if err := s.requireVerified(msg.Source); err != nil {
		return Message{}, err
	}

	msg.Timestamp = s.now().UTC()
	msg.ID = s.messageID(msg.Timestamp)
	if err := s.put(messagesBucket, messageKey(msg), msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// requireVerified enforces the SES sandbox rule: the sender address or its
// domain must be a verified identity.
func (s *Service) requireVerified(source string) error {
	address := addressOf(source)
	if _, ok, err := s.loadIdentity(address); err != nil {
		return err
	} else if ok {
		return nil
	}
	if domain := domainOf(address); domain != "" {
		if _, ok, err := s.loadIdentity(domain); err != nil {
			return err
		} else if ok {
			return nil
		}
	}
	return &apierror.Error{
		StatusCode: http.StatusBadRequest,
		Code:       "MessageRejected",
		Message: fmt.Sprintf("Email address is not verified. The following identities failed the check in region %s: %s "+
			"(verify it with `aws ses verify-email-identity --email-address %s`)", strings.ToUpper(region), address, address),
	}
}

// Messages returns captured emails matching the filter, oldest first.
func (s *Service) Messages(filter MessageFilter) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Message, 0)
	err := s.metadata.Scan(messagesBucket, "", func(_, v []byte) error {
		var msg Message
		if json.Unmarshal(v, &msg) == nil && filter.Matches(msg) {
			items = append(items, msg)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	if filter.Limit > 0 && len(items) > filter.Limit {
		items = items[len(items)-filter.Limit:]
	}
	return items, nil
}

// Message returns one captured email by MessageId.
func (s *Service) Message(id string) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *Message
	err := s.metadata.Scan(messagesBucket, "", func(_, v []byte) error {
		var msg Message
		if found == nil && json.Unmarshal(v, &msg) == nil && msg.ID == id {
			found = &msg
		}
		return nil
	})
	if err != nil {
		return Message{}, internal(err)
	}
	if found == nil {
		return Message{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "MessageNotFound", Message: "no captured email has MessageId " + id}
	}
	return *found, nil
}

// ClearMessages empties the inbox and returns how many emails it held.
// Identities and templates are kept.
func (s *Service) ClearMessages() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	if err := s.metadata.Scan(messagesBucket, "", func(_, _ []byte) error { count++; return nil }); err != nil {
		return 0, internal(err)
	}
	if err := s.metadata.DeletePrefix(messagesBucket, ""); err != nil {
		return 0, internal(err)
	}
	return count, nil
}

// SentCount is how many emails the inbox holds, for quota reports.
func (s *Service) SentCount() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	if err := s.metadata.Scan(messagesBucket, "", func(_, _ []byte) error { count++; return nil }); err != nil {
		return 0, internal(err)
	}
	return count, nil
}

func (s *Service) loadIdentity(name string) (Identity, bool, error) {
	raw, err := s.metadata.Get(identitiesBucket, identityKey(name))
	if err != nil {
		return Identity{}, false, internal(err)
	}
	if raw == nil {
		return Identity{}, false, nil
	}
	var identity Identity
	if err := json.Unmarshal(raw, &identity); err != nil {
		return Identity{}, false, internal(err)
	}
	return identity, true, nil
}

func (s *Service) loadTemplate(name string) (Template, bool, error) {
	raw, err := s.metadata.Get(templatesBucket, name)
	if err != nil {
		return Template{}, false, internal(err)
	}
	if raw == nil {
		return Template{}, false, nil
	}
	var t Template
	if err := json.Unmarshal(raw, &t); err != nil {
		return Template{}, false, internal(err)
	}
	return t, true, nil
}

func (s *Service) put(bucket, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(bucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

// messageID builds a MessageId that sorts by send time, so the inbox reads
// in order and IDs never collide within one process.
func (s *Service) messageID(at time.Time) string {
	s.seq++
	return fmt.Sprintf("%016x-%06d-%s", at.UnixNano(), s.seq, randomToken()[:8])
}

// messageKey orders the inbox chronologically: bbolt scans keys in order.
func messageKey(msg Message) string {
	return msg.ID
}

func identityKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func validateTemplate(t Template) error {
	if strings.TrimSpace(t.Name) == "" {
		return invalidParameter("TemplateName is required")
	}
	if t.Subject == "" {
		return invalidParameter("Template SubjectPart is required")
	}
	if t.Text == "" && t.HTML == "" {
		return invalidParameter("Template needs a TextPart or an HtmlPart")
	}
	return nil
}

func templateNotFound(name string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "TemplateDoesNotExist", Message: "template does not exist: " + name}
}

func invalidParameter(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterValue", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func randomToken() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
