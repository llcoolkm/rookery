package core

import (
	"encoding/json"
	"testing"
)

func TestExtensionsAndExactNumbers(t *testing.T) {
	// Above float64's integer range, and deliberately noncanonical spellings.
	input := []byte(`{"op":"send","data":{"counter":9007199254740993,"fraction":1.2300e+04},"future":{"counter":9007199254740995}}`)
	var request RPCRequest
	if err := json.Unmarshal(input, &request); err != nil {
		t.Fatal(err)
	}
	want := `{"counter":9007199254740993,"fraction":1.2300e+04}`
	if string(request.Data) != want {
		t.Fatalf("data changed: %s", request.Data)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var command Command
	if err := json.Unmarshal(encoded, &command); err != nil {
		t.Fatal(err)
	}
	if string(command.Data) != want || string(command.Extra["future"]) != `{"counter":9007199254740995}` {
		t.Fatalf("forwarded JSON changed: %s", encoded)
	}
}

func TestDetailPresenceAndReservedFields(t *testing.T) {
	for _, input := range []string{`{"detail":"","hops":0}`, `{"hops":0}`} {
		var message Message
		if err := json.Unmarshal([]byte(input), &message); err != nil {
			t.Fatal(err)
		}
		// Extensions cannot override fields the typed model deliberately omits.
		message.Extra["reply_id"] = json.RawMessage(`"forged"`)
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		_, detail := fields["detail"]
		if detail != (message.Detail != nil) {
			t.Fatalf("detail presence changed: %s", encoded)
		}
		if detail && string(fields["detail"]) != `""` {
			t.Fatalf("empty detail changed: %s", encoded)
		}
		if _, present := fields["reply_id"]; present {
			t.Fatal("extension overrode known field")
		}
	}
}
