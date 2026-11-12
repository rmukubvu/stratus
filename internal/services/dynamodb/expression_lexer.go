package dynamodb

import (
	"fmt"
	"strings"
	"unicode"
)

// The DynamoDB expression languages (condition, filter, key condition,
// update and projection expressions) share one lexer.

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokName   // #alias
	tokValue  // :placeholder
	tokNumber // a list index
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
	tokDot
	tokEq
	tokNe
	tokLt
	tokLe
	tokGt
	tokGe
	tokPlus
	tokMinus
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '#' || c == ':':
			start := i
			i++
			for i < len(src) && isNameByte(src[i]) {
				i++
			}
			if i == start+1 {
				return nil, syntaxError(src, string(c), start)
			}
			kind := tokName
			if c == ':' {
				kind = tokValue
			}
			out = append(out, token{kind, src[start:i], start})
			continue
		case isDigit(c):
			start := i
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			out = append(out, token{tokNumber, src[start:i], start})
			continue
		case isNameStart(c):
			start := i
			for i < len(src) && isNameByte(src[i]) {
				i++
			}
			out = append(out, token{tokIdent, src[start:i], start})
			continue
		}
		start := i
		two := ""
		if i+1 < len(src) {
			two = src[i : i+2]
		}
		switch {
		case two == "<>":
			out, i = append(out, token{tokNe, two, start}), i+2
		case two == "<=":
			out, i = append(out, token{tokLe, two, start}), i+2
		case two == ">=":
			out, i = append(out, token{tokGe, two, start}), i+2
		default:
			kinds := map[byte]tokenKind{'(': tokLParen, ')': tokRParen, '[': tokLBracket, ']': tokRBracket, ',': tokComma,
				'.': tokDot, '=': tokEq, '<': tokLt, '>': tokGt, '+': tokPlus, '-': tokMinus}
			kind, ok := kinds[c]
			if !ok {
				return nil, &exprError{msg: fmt.Sprintf("Invalid character encountered; character: %q, near: %q", string(c), near(src, start))}
			}
			out, i = append(out, token{kind, string(c), start}), i+1
		}
	}
	return append(out, token{tokEOF, "<EOF>", len(src)}), nil
}

func isDigit(c byte) bool     { return c >= '0' && c <= '9' }
func isNameStart(c byte) bool { return c == '_' || unicode.IsLetter(rune(c)) }
func isNameByte(c byte) bool  { return isNameStart(c) || isDigit(c) }

// exprError is an invalid expression; the caller prefixes it with the
// expression's kind ("Invalid ConditionExpression: ...").
// exprError is an expression error. A counted error is a semantic one that
// DynamoDB reports as "1 validation error detected: Invalid <kind>: <msg>"
// in a ConditionExpression.
type exprError struct {
	msg     string
	counted bool
}

func (e *exprError) Error() string { return e.msg }

func syntaxError(src, tok string, pos int) error {
	return &exprError{msg: fmt.Sprintf("Syntax error; token: %q, near: %q", tok, near(src, pos))}
}

func near(src string, pos int) string {
	start, end := pos-4, pos+8
	if start < 0 {
		start = 0
	}
	if end > len(src) {
		end = len(src)
	}
	return strings.TrimSpace(src[start:end])
}

// reservedWords are DynamoDB reserved words most often used by mistake as
// attribute names; using one bare in an expression is an error.
var reservedWords = toSet(`ABORT ABSOLUTE ACTION ADD AFTER AGENT AGGREGATE ALL ALLOCATE ALTER ANALYZE AND ANY ARCHIVE ARE ARRAY AS ASC
ASCII ASENSITIVE ASSERTION ASYMMETRIC AT ATOMIC ATTACH ATTRIBUTE AUTH AUTHORIZATION AUTHORIZE AUTO AVG BACK BACKUP BASE BATCH BEFORE
BEGIN BETWEEN BIGINT BINARY BIT BLOB BLOCK BOOLEAN BOTH BREADTH BUCKET BULK BY BYTE CALL CALLED CALLING CAPACITY CASCADE CASCADED
CASE CAST CATALOG CHAR CHARACTER CHECK CLASS CLOB CLOSE CLUSTER CLUSTERED CLUSTERING CLUSTERS COALESCE COLLATE COLLATION COLLECTION
COLUMN COLUMNS COMBINE COMMENT COMMIT COMPACT COMPILE COMPRESS CONDITION CONFLICT CONNECT CONNECTION CONSISTENCY CONSISTENT
CONSTRAINT CONSTRAINTS CONSTRUCTOR CONSUMED CONTINUE CONVERT COPY CORRESPONDING COUNT COUNTER CREATE CROSS CUBE CURRENT CURSOR CYCLE
DATA DATABASE DATE DATETIME DAY DEALLOCATE DEC DECIMAL DECLARE DEFAULT DEFERRABLE DEFERRED DEFINE DEFINED DEFINITION DELETE
DELIMITED DEPTH DEREF DESC DESCRIBE DESCRIPTOR DETACH DETERMINISTIC DIAGNOSTICS DIRECTORIES DISABLE DISCONNECT DISTINCT DISTRIBUTE
DO DOMAIN DOUBLE DROP DUMP DURATION DYNAMIC EACH ELEMENT ELSE ELSEIF EMPTY ENABLE END EQUAL EQUALS ERROR ESCAPE ESCAPED EVAL
EVALUATE EXCEEDED EXCEPT EXCEPTION EXCEPTIONS EXCLUSIVE EXEC EXECUTE EXISTS EXIT EXPLAIN EXPLODE EXPORT EXPRESSION EXTENDED
EXTERNAL EXTRACT FAIL FALSE FAMILY FETCH FIELDS FILE FILTER FILTERING FINAL FINISH FIRST FIXED FLATTERN FLOAT FOR FORCE FOREIGN
FORMAT FORWARD FOUND FREE FROM FULL FUNCTION FUNCTIONS GENERAL GENERATE GET GLOB GLOBAL GO GOTO GRANT GREATER GROUP GROUPING
HANDLER HASH HAVE HAVING HEAP HIDDEN HOLD HOUR IDENTIFIED IDENTITY IF IGNORE IMMEDIATE IMPORT IN INCLUDING INCLUSIVE INCREMENT
INCREMENTAL INDEX INDEXED INDEXES INDICATOR INFINITE INITIALLY INLINE INNER INNTER INOUT INPUT INSENSITIVE INSERT INSTEAD INT
INTEGER INTERSECT INTERVAL INTO INVALIDATE IS ISOLATION ITEM ITEMS ITERATE JOIN KEY KEYS LAG LANGUAGE LARGE LAST LATERAL LEAD
LEADING LEAVE LEFT LENGTH LESS LEVEL LIKE LIMIT LIMITED LINES LIST LOAD LOCAL LOCALTIME LOCALTIMESTAMP LOCATION LOCATOR LOCK
LOCKS LOG LOGED LONG LOOP LOWER MAP MATCH MATERIALIZED MAX MAXLEN MEMBER MERGE METHOD METRICS MIN MINUS MINUTE MISSING MOD MODE
MODIFIES MODIFY MODULE MONTH MULTI MULTISET NAME NAMES NATIONAL NATURAL NCHAR NCLOB NEW NEXT NO NONE NOT NULL NULLIF NUMBER
NUMERIC OBJECT OF OFFLINE OFFSET OLD ON ONLINE ONLY OPAQUE OPEN OPERATOR OPTION OR ORDER ORDINALITY OTHER OTHERS OUT OUTER OUTPUT
OVER OVERLAPS OVERRIDE OWNER PAD PARALLEL PARAMETER PARAMETERS PARTIAL PARTITION PARTITIONED PARTITIONS PATH PERCENT PERCENTILE
PERMISSION PERMISSIONS PIPE PIPELINED PLAN POOL POSITION PRECISION PREPARE PRESERVE PRIMARY PRIOR PRIVATE PRIVILEGES PROCEDURE
PROCESSED PROJECT PROJECTION PROPERTY PROVISIONING PUBLIC PUT QUERY QUIT QUORUM RAISE RANDOM RANGE RANK RAW READ READS REAL
REBUILD RECORD RECURSIVE REDUCE REF REFERENCE REFERENCES REFERENCING REGEXP REGION REINDEX RELATIVE RELEASE REMAINDER RENAME
REPEAT REPLACE REQUEST RESET RESIGNAL RESOURCE RESPONSE RESTORE RESTRICT RESULT RETURN RETURNING RETURNS REVERSE REVOKE RIGHT
ROLE ROLES ROLLBACK ROLLUP ROUTINE ROW ROWS RULE RULES SAMPLE SATISFIES SAVE SAVEPOINT SCAN SCHEMA SCOPE SCROLL SEARCH SECOND
SECTION SEGMENT SEGMENTS SELECT SELF SEMI SENSITIVE SEPARATE SEQUENCE SERIALIZABLE SESSION SET SETS SHARD SHARE SHARED SHORT SHOW
SIGNAL SIMILAR SIZE SKEWED SMALLINT SNAPSHOT SOME SOURCE SPACE SPACES SPARSE SPECIFIC SPECIFICTYPE SPLIT SQL SQLCODE SQLERROR
SQLEXCEPTION SQLSTATE SQLWARNING START STATE STATIC STATUS STORAGE STORE STORED STREAM STRING STRUCT STYLE SUB SUBMULTISET
SUBPARTITION SUBSTRING SUBTYPE SUM SUPER SYMMETRIC SYNONYM SYSTEM TABLE TABLESAMPLE TEMP TEMPORARY TERMINATED TEXT THAN THEN
THROUGHPUT TIME TIMESTAMP TIMEZONE TINYINT TO TOKEN TOTAL TOUCH TRAILING TRANSACTION TRANSFORM TRANSLATE TRANSLATION TREAT
TRIGGER TRIM TRUE TRUNCATE TTL TUPLE TYPE UNDER UNDO UNION UNIQUE UNIT UNKNOWN UNLOGGED UNNEST UNPROCESSED UNSIGNED UNTIL UPDATE
UPPER URL USAGE USE USER USERS USING UUID VACUUM VALUE VALUED VALUES VARCHAR VARIABLE VARIANCE VARINT VARYING VIEW VIEWS VIRTUAL
VOID WAIT WHEN WHENEVER WHERE WHILE WINDOW WITH WITHIN WITHOUT WORK WRAPPED WRITE YEAR ZONE`)

func toSet(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}
