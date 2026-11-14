package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const reservedFixture = `{"model":"original","stream":true,"input":[{"type":"reasoning","encrypted_content":"secret"}],"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}]},{"type":"function","name":"other","parameters":{"type":"number"}}]}`

func TestReservedSchemaRestoredOnEveryRetryAttempt(t *testing.T) {
	original := []byte(reservedFixture)
	payload := []byte(strings.Replace(reservedFixture, `"timeout_ms":{"type":"number"}`, `"timeout_ms":{"type":"integer"}`, 1))
	payload = bytes.Replace(payload, []byte(`"model":"original"`), []byte(`"model":"prepared","temperature":0.7`), 1)
	cfg := DefaultConfig()
	cfg.RetryStripReasoning = true
	for _, index := range []int{0, 1} {
		exec := pluginapi.ExecutorRequest{Model: "prepared", Payload: payload, OriginalRequest: original}
		got := retryAttemptBody(cfg, exec, retryAttemptState{index: index, target: RetryTarget{Model: "fallback"}, requested: RetryTarget{Model: "prepared"}})
		if !bytes.Contains(got, []byte(`"timeout_ms":{"type":"number"}`)) {
			t.Fatalf("attempt %d schema not restored: %s", index, got)
		}
		var d map[string]json.RawMessage
		if err := json.Unmarshal(got, &d); err != nil {
			t.Fatal(err)
		}
		if string(d["temperature"]) != "0.7" {
			t.Fatal("lost host payload settings")
		}
		if index == 1 && (string(d["model"]) != `"fallback"` || bytes.Contains(d["input"], []byte("encrypted_content"))) {
			t.Fatal("fallback handling changed")
		}
		if !bytes.Equal(restoreReservedCollaborationSchemas(got, original), got) {
			t.Fatal("restoration not idempotent")
		}
	}
}

func TestReservedSchemaNoOpAndFailOpen(t *testing.T) {
	for _, original := range [][]byte{nil, []byte("broken"), []byte(`{}`), []byte(reservedFixture)} {
		payload := []byte(reservedFixture)
		if !bytes.Equal(restoreReservedCollaborationSchemas(payload, original), payload) {
			t.Fatal("unchanged/invalid original should preserve bytes")
		}
	}
	changed := []byte(strings.ReplaceAll(reservedFixture, `"type":"number"`, `"type":"integer"`))
	got := restoreReservedCollaborationSchemas(changed, []byte(reservedFixture))
	if !bytes.Contains(got, []byte(`"name":"other","parameters":{"type":"integer"}`)) {
		t.Fatal("unrelated tool changed")
	}
	// Never add a missing reserved namespace, or guess between duplicates.
	for _, payload := range []string{`{"tools":[]}`, `{"tools":[{"type":"namespace","name":"collaboration"},{"type":"namespace","name":"collaboration"}]}`} {
		if string(restoreReservedCollaborationSchemas([]byte(payload), []byte(reservedFixture))) != payload {
			t.Fatal("ambiguous/missing tool changed")
		}
	}
}

func TestReservedAdditionalTools(t *testing.T) {
	original := []byte(`{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}]}]}]}`)
	payload := bytes.ReplaceAll(original, []byte(`"number"`), []byte(`"integer"`))
	got := restoreReservedCollaborationSchemas(payload, original)
	if !bytes.Contains(got, []byte(`"timeout_ms":{"type":"number"}`)) {
		t.Fatalf("history schema not restored: %s", got)
	}
}
