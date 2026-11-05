package core

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Wire objects retain extension fields without decoding numbers through float64.
// Known fields always win, including fields deliberately omitted by omitempty.
// RawMessage retains the JSON value's spelling; encoding/json may compact its
// whitespace on output. It does not promise byte-identical whole documents.
func wireFields(value any) map[string]bool {
	fields := make(map[string]bool)
	typ := reflect.TypeOf(value)
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = true
		}
	}
	return fields
}

func decodeWire(data []byte, value any, extra *map[string]json.RawMessage) error {
	if err := json.Unmarshal(data, value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range wireFields(reflect.ValueOf(value).Elem().Interface()) {
		delete(fields, name)
	}
	*extra = fields
	return nil
}

func encodeWire(value any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	known := wireFields(value)
	for name, raw := range extra {
		if !known[name] {
			fields[name] = raw
		}
	}
	return json.Marshal(fields)
}

func (v *Message) UnmarshalJSON(data []byte) error {
	type wire Message
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = Message(decoded)
	return nil
}
func (v Message) MarshalJSON() ([]byte, error) {
	type wire Message
	return encodeWire(wire(v), v.Extra)
}

func (v *Envelope) UnmarshalJSON(data []byte) error {
	type wire Envelope
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = Envelope(decoded)
	return nil
}
func (v Envelope) MarshalJSON() ([]byte, error) {
	type wire Envelope
	return encodeWire(wire(v), v.Extra)
}

func (v *RPCRequest) UnmarshalJSON(data []byte) error {
	type wire RPCRequest
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = RPCRequest(decoded)
	return nil
}
func (v RPCRequest) MarshalJSON() ([]byte, error) {
	type wire RPCRequest
	return encodeWire(wire(v), v.Extra)
}

func (v *RPCResponse) UnmarshalJSON(data []byte) error {
	type wire RPCResponse
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = RPCResponse(decoded)
	return nil
}
func (v RPCResponse) MarshalJSON() ([]byte, error) {
	type wire RPCResponse
	return encodeWire(wire(v), v.Extra)
}

func (v *Command) UnmarshalJSON(data []byte) error {
	type wire Command
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = Command(decoded)
	return nil
}
func (v Command) MarshalJSON() ([]byte, error) {
	type wire Command
	return encodeWire(wire(v), v.Extra)
}

func (v *Event) UnmarshalJSON(data []byte) error {
	type wire Event
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = Event(decoded)
	return nil
}
func (v Event) MarshalJSON() ([]byte, error) {
	type wire Event
	return encodeWire(wire(v), v.Extra)
}

func (v *Session) UnmarshalJSON(data []byte) error {
	type wire Session
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = Session(decoded)
	return nil
}
func (v Session) MarshalJSON() ([]byte, error) {
	type wire Session
	return encodeWire(wire(v), v.Extra)
}

func (v *DeliveryResult) UnmarshalJSON(data []byte) error {
	type wire DeliveryResult
	var decoded wire
	if err := decodeWire(data, &decoded, &decoded.Extra); err != nil {
		return err
	}
	*v = DeliveryResult(decoded)
	return nil
}
func (v DeliveryResult) MarshalJSON() ([]byte, error) {
	type wire DeliveryResult
	return encodeWire(wire(v), v.Extra)
}
