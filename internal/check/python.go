package check

import (
	"regexp"
	"strings"
	"unicode"
)

// Python finds boto3 and botocore client calls: the services a file creates
// clients for (boto3.client("sqs"), session.client(service_name="s3")) and
// the snake_case methods it calls that one of those services defines.
type Python struct{}

func (Python) Accepts(name string) bool { return strings.HasSuffix(name, ".py") }

var (
	pyClient = regexp.MustCompile(`\.(?:client|resource)\(\s*(?:service_name\s*=\s*)?["']([a-z0-9-]+)["']`)
	pyCall   = regexp.MustCompile(`\.([a-z][a-z0-9_]*)\(`)
)

func (Python) Scan(file string, src []byte) []Call {
	var services []string
	var calls []Call
	for _, m := range pyClient.FindAllSubmatchIndex(src, -1) {
		service := string(src[m[2]:m[3]])
		services = append(services, service)
		if operationsBy(service, snake) == nil {
			calls = append(calls, Call{Service: service, Operation: "*", Location: Location{file, lineOf(src, m[0])}})
		}
	}
	if len(services) == 0 {
		return calls
	}
	tables := make([]map[string]string, len(services))
	for i, s := range services {
		tables[i] = operationsBy(s, snake)
	}
	for _, m := range pyCall.FindAllSubmatchIndex(src, -1) {
		method := string(src[m[2]:m[3]])
		for i, table := range tables {
			if op, ok := table[method]; ok {
				calls = append(calls, Call{Service: services[i], Operation: op, Location: Location{file, lineOf(src, m[0])}})
				break
			}
		}
	}
	return calls
}

// snake spells an operation the way boto3 names its method
// (botocore.xform_name): CreateQueue -> create_queue, ListTagsForResource ->
// list_tags_for_resource, DescribeDBInstances -> describe_db_instances.
func snake(op string) string {
	runes := []rune(op)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prevLower := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || (unicode.IsUpper(runes[i-1]) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
