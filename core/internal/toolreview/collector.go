// Package toolreview collects client tool calls and reviews them without storing
// request bodies, credentials, or provider diagnostics in review metadata.
package toolreview

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

const (
	maxCalls       = contract.MaxToolReviewCalls
	maxBufferBytes = contract.MaxToolReviewBufferBytes
	maxInputBytes  = contract.MaxToolReviewInputBytes
)

// Error contains only a stable code, never provider content or tool arguments.
type Error struct{ Code string }

func (e *Error) Error() string    { return "tool review: " + e.Code }
func safeError(code string) error { return &Error{Code: code} }

// Call is one client-executed function or custom tool. Custom arguments are text,
// not JSON. Index is the stable ordinal in the completed collection.
type Call struct {
	Index     int
	Name      string
	Arguments string
	Custom    bool
}

type collectedCall struct {
	group, slot                   int
	id, callID, name, kind        string
	arguments                     strings.Builder
	hasArguments, fragments, done bool
	responseItemDone              bool
}

// Collector is owned by one response reader. It is not safe for concurrent use.
// Calls may be read only after a legal terminal event. Errors poison collection.
type Collector struct {
	protocol                contract.ProtocolID
	streaming               bool
	observed                bool
	responseEmptyFinal      bool
	expectedCandidates      int
	started                 time.Time
	calls                   map[string]*collectedCall
	ids                     map[string]string
	callIDs                 map[string]string
	geminiFinal             map[int][]string
	groups                  map[int]bool
	blocks                  map[int]string
	anthropicStop           string
	used                    int
	saw, complete, contains bool
	err                     error
}

func NewCollector(protocol contract.ProtocolID, streaming bool) *Collector {
	return &Collector{protocol: protocol, streaming: streaming, expectedCandidates: 1, calls: make(map[string]*collectedCall), ids: make(map[string]string), callIDs: make(map[string]string), geminiFinal: make(map[int][]string), groups: make(map[int]bool), blocks: make(map[int]string)}
}

// SetExpectedCandidates configures Chat n or Gemini candidateCount. The default
// is one. Call this before the first Observe; other protocols do not use it.
func (c *Collector) SetExpectedCandidates(count int) error {
	if c.err != nil {
		return c.err
	}
	if count < 1 {
		return c.fail(safeError("invalid_candidate_count"))
	}
	if c.observed {
		return c.fail(safeError("collector_started"))
	}
	c.expectedCandidates = count
	return nil
}

// Finish records a normal transport EOF, never cancellation or a read failure.
// Chat can end at EOF only when every expected choice has a finish_reason.
// Other protocols must already have supplied their own successful terminal.
func (c *Collector) Finish() error {
	if c.err != nil {
		return c.err
	}
	if c.complete {
		return nil
	}
	if c.streaming && !c.started.IsZero() && time.Since(c.started) > 120*time.Second {
		return c.fail(safeError("collection_timeout"))
	}
	if c.protocol != contract.ProtocolOpenAIChat || !c.streaming || !c.groupsComplete() {
		return c.fail(safeError("incomplete_response"))
	}
	if err := c.finish(); err != nil {
		return c.fail(err)
	}
	return nil
}

func (c *Collector) registerGroup(group int, seen map[int]bool) error {
	if group < 0 || group >= c.expectedCandidates || seen[group] {
		return safeError("invalid_candidates")
	}
	seen[group] = true
	if _, ok := c.groups[group]; !ok {
		if c.complete {
			return safeError("conflicting_call")
		}
		if err := c.reserve(64); err != nil {
			return err
		}
		c.groups[group] = false
	}
	return nil
}

func (c *Collector) groupsComplete() bool {
	if len(c.groups) != c.expectedCandidates {
		return false
	}
	for _, done := range c.groups {
		if !done {
			return false
		}
	}
	return true
}

func (c *Collector) SawTool() bool  { return c.saw }
func (c *Collector) Complete() bool { return c.complete && c.err == nil }

func (c *Collector) fail(err error) error {
	if c.err == nil {
		c.err = err
	}
	c.complete = false
	// A failed collector cannot be recovered; release sensitive buffered data.
	c.calls = nil
	c.ids = nil
	c.callIDs = nil
	c.geminiFinal = nil
	c.blocks = nil
	c.groups = nil
	return c.err
}

func (c *Collector) reserve(n int) error {
	if n < 0 || n > maxBufferBytes-c.used {
		return safeError("buffer_limit")
	}
	c.used += n
	return nil
}

func (c *Collector) call(key string, group, slot int, id, name, kind string) (*collectedCall, error) {
	c.saw, c.contains = true, true
	if c.started.IsZero() {
		c.started = time.Now()
	}
	v := c.calls[key]
	if v == nil {
		if c.complete || c.groups[group] {
			return nil, safeError("conflicting_call")
		}
		if len(c.calls) >= maxCalls {
			return nil, safeError("call_limit")
		}
		if err := c.reserve(256 + len(key)); err != nil {
			return nil, err
		}
		v = &collectedCall{group: group, slot: slot}
		c.calls[key] = v
	}
	for _, field := range []struct {
		target *string
		value  string
	}{{&v.id, id}, {&v.name, name}, {&v.kind, kind}} {
		if field.value == "" {
			continue
		}
		if *field.target != "" && *field.target != field.value {
			return nil, safeError("conflicting_call")
		}
		if *field.target == "" {
			if c.complete || v.done {
				return nil, safeError("conflicting_call")
			}
			if err := c.reserve(len(field.value)); err != nil {
				return nil, err
			}
			*field.target = field.value
		}
	}
	if id != "" {
		if previous, ok := c.ids[id]; ok && previous != key {
			return nil, safeError("conflicting_call")
		}
		c.ids[id] = key
	}
	return v, nil
}

func (c *Collector) appendArguments(v *collectedCall, fragment string) error {
	if c.complete || v.done {
		return safeError("conflicting_call")
	}
	if v.hasArguments && !v.fragments {
		return safeError("conflicting_call")
	}
	// Builders may grow to roughly twice their content; charge for both copies.
	if err := c.reserve(2 * len(fragment)); err != nil {
		return err
	}
	v.arguments.WriteString(fragment)
	v.hasArguments, v.fragments = true, true
	return nil
}

func (c *Collector) snapshotArguments(v *collectedCall, value string) error {
	if v.hasArguments {
		if !equalArguments(v.arguments.String(), value, v.kind == "custom") {
			return safeError("conflicting_call")
		}
		return nil
	}
	if c.complete || v.done {
		return safeError("conflicting_call")
	}
	if err := c.reserve(2 * len(value)); err != nil {
		return err
	}
	v.arguments.WriteString(value)
	v.hasArguments = true
	return nil
}

func validArguments(value string) bool {
	trimmed := strings.TrimSpace(value)
	return len(trimmed) > 0 && trimmed[0] == '{' && strictJSON([]byte(trimmed))
}

func equalArguments(a, b string, custom bool) bool {
	if a == b {
		return true
	}
	if custom || !validArguments(a) || !validArguments(b) {
		return false
	}
	// Use numbers without float conversion, preserving large integer semantics.
	var left, right any
	da, db := json.NewDecoder(strings.NewReader(a)), json.NewDecoder(strings.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&left) != nil || db.Decode(&right) != nil {
		return false
	}
	ja, ea := json.Marshal(left)
	jb, eb := json.Marshal(right)
	return ea == nil && eb == nil && bytes.Equal(ja, jb)
}

func (c *Collector) finishCall(v *collectedCall) error {
	if v.name == "" || v.kind == "" || !v.hasArguments {
		return safeError("incomplete_call")
	}
	if v.kind != "custom" && !validArguments(v.arguments.String()) {
		return safeError("invalid_arguments")
	}
	v.done = true
	return nil
}

func (c *Collector) hasGroupCalls(group int) bool {
	for _, call := range c.calls {
		if call.group == group {
			return true
		}
	}
	return false
}

func (c *Collector) finish() error {
	for _, v := range c.calls {
		if err := c.finishCall(v); err != nil {
			return err
		}
	}
	c.complete = true
	return nil
}

func (c *Collector) Calls() ([]Call, error) {
	if c.err != nil {
		return nil, c.err
	}
	if !c.complete {
		if c.streaming && !c.started.IsZero() && time.Since(c.started) > 120*time.Second {
			return nil, c.fail(safeError("collection_timeout"))
		}
		return nil, safeError("incomplete_response")
	}
	items := make([]*collectedCall, 0, len(c.calls))
	for _, v := range c.calls {
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].group != items[j].group {
			return items[i].group < items[j].group
		}
		return items[i].slot < items[j].slot
	})
	result := make([]Call, len(items))
	for i, v := range items {
		result[i] = Call{Index: i, Name: v.name, Arguments: v.arguments.String(), Custom: v.kind == "custom"}
	}
	return result, nil
}

// Observe accepts a single response JSON or SSE data value, not an SSE envelope.
func (c *Collector) Observe(payload []byte) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	c.observed = true
	c.contains = false
	if c.streaming && !c.complete && !c.started.IsZero() && time.Since(c.started) > 120*time.Second {
		return false, c.fail(safeError("collection_timeout"))
	}
	if len(payload) > maxBufferBytes {
		return false, c.fail(safeError("buffer_limit"))
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		if c.protocol != contract.ProtocolOpenAIChat || !c.streaming {
			return false, c.fail(safeError("incomplete_response"))
		}
		return false, c.Finish()
	}
	trimmed := bytes.TrimSpace(payload)
	geminiArray := c.protocol == contract.ProtocolGoogleGenerateContent && !c.streaming && len(trimmed) > 0 && trimmed[0] == '['
	if !strictJSON(payload) || len(trimmed) == 0 || (trimmed[0] != '{' && !geminiArray) {
		return false, c.fail(safeError("invalid_payload"))
	}
	var err error
	switch c.protocol {
	case contract.ProtocolOpenAIChat:
		err = c.observeChat(payload)
	case contract.ProtocolOpenAIResponses:
		err = c.observeResponses(payload)
	case contract.ProtocolAnthropicMessages:
		err = c.observeAnthropic(payload)
	case contract.ProtocolGoogleGenerateContent:
		if geminiArray {
			err = c.observeGeminiArray(payload)
		} else {
			err = c.observeGemini(payload, c.streaming)
		}
	default:
		err = safeError("unsupported_protocol")
	}
	if err != nil {
		return c.contains, c.fail(err)
	}
	return c.contains, nil
}

func decodePayload(payload []byte, target any) error {
	if !exactFieldNames(payload, reflect.TypeOf(target)) || json.Unmarshal(payload, target) != nil {
		return safeError("invalid_payload")
	}
	return nil
}
