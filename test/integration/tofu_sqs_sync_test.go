// Package integration — but this file is NOT integration-tagged on purpose,
// exactly like realm_sync_test.go next to it.
//
// WHY THIS TEST EXISTS
// --------------------
// The SQS queues are now declared twice:
//
//   - deploy/localstack/init/10-queues.py — what docker-compose actually runs
//     on boot, with boto3, and what test/integration/sqs_policy_test.go asserts
//     against the live server.
//   - deploy/tofu/*.tf — the same queues expressed as infrastructure, which is
//     the only form that can carry the resource policy, the IAM split and the
//     CMK that LocalStack stores-but-does-not-enforce.
//
// Two declarations of one truth drift. This repository has already paid for
// that lesson: the Keycloak realm and the LocalStack init each exist as a copy
// inside the Helm chart, and both copies HAD diverged before realm_sync_test.go
// made it impossible. The cost of prevention here is one cheap parse.
//
// The comparison is PER QUEUE, not per attribute: retention is deliberately
// different between the work queue (4 days) and the evidence queues (14 days),
// so a flat "first MessageRetentionPeriod wins" scan would be wrong. Each
// ensure_queue(...) block in the script is matched against the tofu resource
// that declares the same queue name.
package integration

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pyQueue is one ensure_queue("name", {...}) block from the boto script.
type pyQueue struct {
	name       string
	body       string
	retention  string // resolved, including one level of named constant
	visibility string
	longPoll   string
	maxReceive string
	maxSize    string
	dedup      string
	sse        string
}

// resolvePythonConstant resolves a named constant one level, e.g. the
// MessageRetentionPeriod value MAX_RETENTION -> "1209600". Literal quoted
// values are returned as-is.
func resolvePythonConstant(src, value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, ",")
	if v, err := strconv.Unquote(value); err == nil {
		return v
	}
	// Not a literal: treat it as a constant name and resolve one definition.
	reDef := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(value) + `\s*=\s*"([^"]+)"`)
	if d := reDef.FindStringSubmatch(src); d != nil {
		return d[1]
	}
	return value
}

// parsePythonQueues extracts each queue block so values can be compared per
// queue rather than by first occurrence.
func parsePythonQueues(t *testing.T, src string) map[string]pyQueue {
	t.Helper()

	// Anchor on `ensure_queue(` and capture up to the closing `)` that sits at
	// column 0, which is how the script is formatted:
	//
	//   ensure_queue(
	//       "wager-transactions.fifo",
	//       { ... "MessageRetentionPeriod": "345600", ... },
	//   )
	//
	// The dict's own `},` is indented, so it does not terminate the match.
	reBlock := regexp.MustCompile(`(?ms)^\w*\s*=?\s*ensure_queue\(\s*"([^"]+)",\s*\{(.*?)^\)`)
	blocks := reBlock.FindAllStringSubmatch(src, -1)
	if len(blocks) == 0 {
		t.Fatalf("10-queues.py has no ensure_queue(\"name\", {...}) blocks this test can read;\n" +
			"update test/integration/tofu_sqs_sync_test.go and deploy/tofu/ together")
	}

	attr := func(body, name string) string {
		re := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `":\s*([^,\n]+)`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return resolvePythonConstant(src, m[1])
	}

	out := make(map[string]pyQueue, len(blocks))
	for _, b := range blocks {
		name, body := b[1], b[2]
		out[name] = pyQueue{
			name:       name,
			body:       body,
			retention:  attr(body, "MessageRetentionPeriod"),
			visibility: attr(body, "VisibilityTimeout"),
			longPoll:   attr(body, "ReceiveMessageWaitTimeSeconds"),
			maxSize:    attr(body, "MaximumMessageSize"),
			dedup:      attr(body, "ContentBasedDeduplication"),
			sse:        attr(body, "SqsManagedSseEnabled"),
			maxReceive: func() string {
				m := regexp.MustCompile(`"maxReceiveCount":\s*(\d+)`).FindStringSubmatch(body)
				if m == nil {
					return ""
				}
				return m[1]
			}(),
		}
	}
	return out
}

// tofuNumberDefault extracts the `default = <n>` of a variable block.
func tofuNumberDefault(t *testing.T, src, variable string) int64 {
	t.Helper()

	re := regexp.MustCompile(`(?s)variable\s+"` + regexp.QuoteMeta(variable) + `"\s*\{(.*?)\n\}`)
	block := re.FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("deploy/tofu/variables.tf has no variable %q", variable)
	}
	reDef := regexp.MustCompile(`default\s*=\s*(\d+)`)
	d := reDef.FindStringSubmatch(block[1])
	if d == nil {
		t.Fatalf("variable %q has no numeric default", variable)
	}
	n, err := strconv.ParseInt(d[1], 10, 64)
	if err != nil {
		t.Fatalf("variable %q default is not an integer: %v", variable, err)
	}
	return n
}

// tofuResourceBlock returns the body of resource "type" "name" { ... }.
func tofuResourceBlock(t *testing.T, src, typ, name string) string {
	t.Helper()

	re := regexp.MustCompile(`(?s)resource\s+"` + regexp.QuoteMeta(typ) + `"\s+"` + regexp.QuoteMeta(name) + `"\s*\{(.*?)\n\}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("deploy/tofu/sqs.tf has no resource %s.%s", typ, name)
	}
	return m[1]
}

func TestTofuSQSMatchesLocalstackInit(t *testing.T) {
	t.Parallel()

	pyRaw, err := os.ReadFile("../../deploy/localstack/init/10-queues.py")
	if err != nil {
		t.Fatalf("read 10-queues.py: %v", err)
	}
	varsRaw, err := os.ReadFile("../../deploy/tofu/variables.tf")
	if err != nil {
		t.Fatalf("read variables.tf: %v", err)
	}
	sqsRaw, err := os.ReadFile("../../deploy/tofu/sqs.tf")
	if err != nil {
		t.Fatalf("read sqs.tf: %v", err)
	}
	py, vars, sqs := string(pyRaw), string(varsRaw), string(sqsRaw)

	queues := parsePythonQueues(t, py)

	// The tofu variables the shared attributes must resolve to.
	visibility := tofuNumberDefault(t, vars, "visibility_timeout_seconds")
	longPoll := tofuNumberDefault(t, vars, "long_poll_seconds")
	workRetention := tofuNumberDefault(t, vars, "work_retention_seconds")
	evidenceRetention := tofuNumberDefault(t, vars, "evidence_retention_seconds")
	maxReceive := tofuNumberDefault(t, vars, "max_receive_count")
	maxSize := tofuNumberDefault(t, vars, "max_message_size_bytes")

	// Each queue, with the tofu resource that declares it and the retention it
	// must carry. The work queue is the odd one out on purpose.
	cases := []struct {
		pyName       string
		tofuResource string
		retention    int64
		retentionVar string // which tofu variable carries this queue's retention
	}{
		{"wager-transactions-dlq.fifo", "dlq", evidenceRetention, "evidence_retention_seconds"},
		{"wager-transactions.fifo", "work", workRetention, "work_retention_seconds"},
		{"integration-events.fifo", "events", evidenceRetention, "evidence_retention_seconds"},
	}

	for _, c := range cases {
		q, ok := queues[c.pyName]
		if !ok {
			t.Errorf("10-queues.py no longer creates %q", c.pyName)
			continue
		}
		body := tofuResourceBlock(t, sqs, "aws_sqs_queue", c.tofuResource)

		// The boto script must provision the value, and the tofu resource must
		// be *wired* to the variable that carries it. The tofu body holds
		// `var.<name>`, not a literal: asserting the literal there would force
		// the module to abandon the variable it exists to centralise. So the
		// two halves are checked separately.
		check := func(attr, pyVal string, want int64, tofuVar string) {
			if pyVal != strconv.FormatInt(want, 10) {
				t.Errorf("%s: %s diverged — 10-queues.py has %q, tofu variable %s is %d.\n"+
					"  fix both: deploy/localstack/init/10-queues.py and deploy/tofu/variables.tf",
					c.pyName, attr, pyVal, tofuVar, want)
			}
			if !regexp.MustCompile(regexp.QuoteMeta("var." + tofuVar)).MatchString(body) {
				t.Errorf("%s: tofu resource aws_sqs_queue.%s does not use var.%s.\n"+
					"  It is expected to wire the attribute to that variable, not inline a value.",
					c.pyName, c.tofuResource, tofuVar)
			}
		}

		check("VisibilityTimeout", q.visibility, visibility, "visibility_timeout_seconds")
		check("ReceiveMessageWaitTimeSeconds", q.longPoll, longPoll, "long_poll_seconds")
		check("MessageRetentionPeriod", q.retention, c.retention, c.retentionVar)
		check("MaximumMessageSize", q.maxSize, maxSize, "max_message_size_bytes")
	}

	// Redrive: only the work queue has one, and only it must.
	work, ok := queues["wager-transactions.fifo"]
	if !ok {
		t.Fatalf("work queue missing from 10-queues.py")
	}
	if work.maxReceive != strconv.FormatInt(maxReceive, 10) {
		t.Errorf("maxReceiveCount diverged: 10-queues.py has %q, tofu variable max_receive_count is %d.\n"+
			"  fix both: deploy/localstack/init/10-queues.py and deploy/tofu/variables.tf",
			work.maxReceive, maxReceive)
	}
	if !regexp.MustCompile(`aws_sqs_queue_redrive_policy`).MatchString(sqs) {
		t.Errorf("10-queues.py sets a RedrivePolicy but deploy/tofu/sqs.tf declares no redrive policy")
	}

	// Dedup semantics: the events queue must be content-based OFF, because the
	// relay supplies the event id as the MessageDeduplicationId.
	if ev, ok := queues["integration-events.fifo"]; ok {
		if ev.dedup != "false" {
			t.Errorf("integration-events.fifo ContentBasedDeduplication is %q in 10-queues.py; "+
				"it must stay false (dedup by event id, never by payload hash)", ev.dedup)
		}
	}
	if !regexp.MustCompile(`content_based_deduplication\s*=\s*false`).MatchString(sqs) {
		t.Errorf("deploy/tofu/sqs.tf does not set content_based_deduplication = false on the events queue")
	}
}

// The queue names are what the service looks up by; a typo on either side means
// the consumer polls a queue that the other side never created.
func TestTofuSQSQueueNamesMatchLocalstackInit(t *testing.T) {
	t.Parallel()

	pyRaw, err := os.ReadFile("../../deploy/localstack/init/10-queues.py")
	if err != nil {
		t.Fatalf("read 10-queues.py: %v", err)
	}
	sqsRaw, err := os.ReadFile("../../deploy/tofu/sqs.tf")
	if err != nil {
		t.Fatalf("read sqs.tf: %v", err)
	}
	py, sqs := string(pyRaw), string(sqsRaw)

	names := []string{
		"wager-transactions-dlq.fifo",
		"wager-transactions.fifo",
		"integration-events.fifo",
	}

	for _, name := range names {
		if !strings.Contains(py, `"`+name+`"`) {
			t.Errorf("10-queues.py no longer creates %q", name)
		}
		if !strings.Contains(sqs, name) {
			t.Errorf("deploy/tofu/sqs.tf does not declare %q", name)
		}
	}
}
