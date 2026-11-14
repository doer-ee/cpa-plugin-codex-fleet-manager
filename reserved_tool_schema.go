package main

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// CPA's plugin finalizer may normalize number to integer before CFM sees the
// request. OpenAI validates reserved collaboration tools against exact schemas.
// Recover only existing function parameters from the untouched client request;
// do not replace tools, models, instructions, or translated request envelopes.
// Missing, ambiguous, or malformed originals leave the payload unchanged.
func restoreReservedCollaborationSchemas(payload, original []byte) []byte {
	if len(original) == 0 || bytes.Equal(payload, original) {
		return payload
	}
	var current, source map[string]json.RawMessage
	if json.Unmarshal(payload, &current) != nil || json.Unmarshal(original, &source) != nil {
		return payload
	}
	changed := restoreCollaborationToolList(current, source)
	var inputs, originalInputs []map[string]json.RawMessage
	if json.Unmarshal(current["input"], &inputs) == nil && json.Unmarshal(source["input"], &originalInputs) == nil && len(inputs) == len(originalInputs) {
		inputChanged := false
		for i := range inputs {
			var typ, originalType string
			_ = json.Unmarshal(inputs[i]["type"], &typ)
			_ = json.Unmarshal(originalInputs[i]["type"], &originalType)
			if typ == "additional_tools" && originalType == typ && restoreCollaborationToolList(inputs[i], originalInputs[i]) {
				inputChanged = true
			}
		}
		if inputChanged {
			current["input"], _ = json.Marshal(inputs)
			changed = true
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(current)
	if err != nil {
		return payload
	}
	return out
}

func restoreCollaborationToolList(current, original map[string]json.RawMessage) bool {
	var tools, sources []map[string]json.RawMessage
	if json.Unmarshal(current["tools"], &tools) != nil || json.Unmarshal(original["tools"], &sources) != nil {
		return false
	}
	find := func(list []map[string]json.RawMessage, typ, name string) map[string]json.RawMessage {
		var match map[string]json.RawMessage
		for _, item := range list {
			var t, n string
			_ = json.Unmarshal(item["type"], &t)
			_ = json.Unmarshal(item["name"], &n)
			if t == typ && n == name {
				if match != nil {
					return nil
				}
				match = item
			}
		}
		return match
	}
	namespace := find(tools, "namespace", "collaboration")
	source := find(sources, "namespace", "collaboration")
	if namespace == nil || source == nil {
		return false
	}
	var functions, originals []map[string]json.RawMessage
	if json.Unmarshal(namespace["tools"], &functions) != nil || json.Unmarshal(source["tools"], &originals) != nil {
		return false
	}
	changed := false
	for _, fn := range functions {
		var name, typ string
		_ = json.Unmarshal(fn["name"], &name)
		_ = json.Unmarshal(fn["type"], &typ)
		if typ != "function" || name == "" || find(functions, typ, name) == nil {
			continue
		}
		original := find(originals, typ, name)
		if original == nil {
			continue
		}
		var before, after map[string]any
		decode := func(raw []byte, value any) error {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			return decoder.Decode(value)
		}
		if !json.Valid(original["parameters"]) || !json.Valid(fn["parameters"]) || decode(original["parameters"], &before) != nil || before == nil || decode(fn["parameters"], &after) != nil || after == nil {
			continue
		}
		if !reflect.DeepEqual(before, after) {
			fn["parameters"] = append(json.RawMessage(nil), original["parameters"]...)
			changed = true
		}
	}
	if changed {
		namespace["tools"], _ = json.Marshal(functions)
		current["tools"], _ = json.Marshal(tools)
	}
	return changed
}
