package builtintools

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func stagedFixture() State {
	return State{Model: "main", ServiceID: "service_original", RoutingModel: "upstream_original", History: []any{Object{"role": "user", "content": "original history"}}, Replacements: map[string][]any{"ws_tool_staged": {Object{"type": "function_call_output", "call_id": "original", "output": "original result"}}}, Images: map[string][]string{"original": {"original image"}}}
}

func requireInvisiblePublication(t *testing.T, store *Store) {
	t.Helper()
	for _, body := range []Object{{"model": "main", "previous_response_id": "resp_tool_staged", "input": "continue"}, {"model": "main", "input": []any{Object{"id": "ws_tool_staged", "type": "web_search_call"}}}} {
		if _, err := store.Prepare("principal", Clone(body)); err == nil {
			t.Fatalf("unpublished reference resolved: %v", body)
		}
	}
}

func TestPublicationStagesCompleteImmutableStateWithoutVisibility(t *testing.T) {
	store := &Store{}
	state := stagedFixture()
	publication, err := store.Stage("principal", "resp_tool_staged", state)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.Discard()
	requireInvisiblePublication(t, store)
	// The immutable publication includes all continuation data and item indexes,
	// not a later lookup of mutable caller-owned maps.
	state.ServiceID = "service_mutated"
	state.RoutingModel = "mutated"
	Map(state.History[0])["content"] = "mutated"
	state.Replacements["ws_tool_staged"] = nil
	state.Images["original"] = nil
	if err := publication.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"resp_tool_staged", "ws_tool_staged"} {
		got, err := store.Get("principal", id)
		if err != nil || got.ServiceID != "service_original" || got.RoutingModel != "upstream_original" || !strings.Contains(Text(got.History), "original history") || len(got.Replacements["ws_tool_staged"]) != 1 || len(got.Images["original"]) != 1 {
			t.Fatalf("incomplete staged state=%+v err=%v", got, err)
		}
		if _, err := store.Get("other-principal", id); err == nil {
			t.Fatal("cross-principal publication")
		}
	}
	if err := publication.Commit(); err == nil {
		t.Fatal("publication committed twice")
	}
}

func TestPublicationDiscardAndConcurrentDecision(t *testing.T) {
	for i := 0; i < 32; i++ {
		store := &Store{}
		publication, err := store.Stage("principal", "resp_tool_staged", stagedFixture())
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		committed := false
		go func() { defer wg.Done(); committed = publication.Commit() == nil }()
		go func() { defer wg.Done(); publication.Discard() }()
		wg.Wait()
		if committed {
			if _, err := store.Get("principal", "ws_tool_staged"); err != nil {
				t.Fatal("committed item index missing")
			}
		} else {
			requireInvisiblePublication(t, store)
		}
		if publication.Commit() == nil {
			t.Fatal("terminal publication reused")
		}
	}
}

type finishFlushFailure struct {
	*httptest.ResponseRecorder
	streaming bool
}

func (w finishFlushFailure) FlushError() error {
	if !w.streaming || strings.Contains(w.Body.String(), "event: response.completed\n") {
		return io.ErrClosedPipe
	}
	return nil
}

func TestRunnerDoesNotPublishBeforeFinishAndFlush(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "sse"}[streaming], func(t *testing.T) {
			store := &Store{}
			writer := finishFlushFailure{httptest.NewRecorder(), streaming}
			runner := Runner{Store: store, Model: func(context.Context, Object) (Object, error) { return modelResponse(message("text")), nil }}
			err := runner.Run(context.Background(), writer, "principal", Object{"model": "main", "input": "hello", "stream": streaming}, nil)
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("finish flush failure was swallowed: %v", err)
			}
			if len(store.entries) != 0 || len(store.items) != 0 {
				t.Fatal("failed finish published continuation state")
			}
		})
	}
}
