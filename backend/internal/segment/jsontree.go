package segment

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

// The rule is parsed into a small JSON tree with json.Decoder.Token instead of
// being unmarshalled into structs. Owning the walk is what makes decoding
// strict everywhere: struct decoding with DisallowUnknownFields does not reach
// into custom UnmarshalJSON implementations of union members, silently keeps
// the last of duplicate keys, and accepts null for any field. Here every
// object of every variant is checked against its own property list, duplicate
// keys are errors, and nesting depth and input size are bounded.

type jsonKind uint8

const (
	jsonObject jsonKind = iota + 1
	jsonArray
	jsonString
	jsonNumber
	jsonBool
	jsonNull
)

type jsonNode struct {
	kind jsonKind
	str  string      // jsonString
	num  json.Number // jsonNumber
	arr  []*jsonNode // jsonArray
	keys []string    // jsonObject, in input order
	obj  map[string]*jsonNode
}

func (k jsonKind) String() string {
	switch k {
	case jsonObject:
		return "object"
	case jsonArray:
		return "array"
	case jsonString:
		return "string"
	case jsonNumber:
		return "number"
	case jsonBool:
		return "boolean"
	default:
		return "null"
	}
}

// parseJSON parses exactly one JSON value spanning the whole input.
func parseJSON(data []byte) (*jsonNode, *FieldError) {
	if len(data) > MaxRuleBytes {
		return nil, &FieldError{Code: CodeTooLarge, Message: "rule exceeds " + strconv.Itoa(MaxRuleBytes) + " bytes"}
	}
	if !utf8.Valid(data) {
		return nil, &FieldError{Code: CodeMalformedJSON, Message: "input is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	p := treeParser{dec: dec}
	root, ferr := p.value("", 1)
	if ferr != nil {
		return nil, ferr
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &FieldError{Code: CodeTrailingData, Message: "unexpected data after the JSON value"}
	}
	return root, nil
}

type treeParser struct{ dec *json.Decoder }

func (p *treeParser) syntaxError(path string, err error) *FieldError {
	msg := "malformed JSON"
	var se *json.SyntaxError
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		msg = "unexpected end of JSON input"
	case errors.As(err, &se):
		msg = "malformed JSON at byte offset " + strconv.FormatInt(se.Offset, 10)
	}
	return &FieldError{Path: path, Code: CodeMalformedJSON, Message: msg}
}

func (p *treeParser) value(path string, depth int) (*jsonNode, *FieldError) {
	tok, err := p.dec.Token()
	if err != nil {
		return nil, p.syntaxError(path, err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if t != '{' && t != '[' {
			return nil, p.syntaxError(path, nil)
		}
		if depth > maxJSONDepth {
			return nil, &FieldError{Path: path, Code: CodeTooDeep, Message: "JSON nesting exceeds " + strconv.Itoa(maxJSONDepth) + " levels"}
		}
		if t == '{' {
			return p.object(path, depth)
		}
		return p.array(path, depth)
	case string:
		return &jsonNode{kind: jsonString, str: t}, nil
	case json.Number:
		return &jsonNode{kind: jsonNumber, num: t}, nil
	case bool:
		return &jsonNode{kind: jsonBool}, nil
	case nil:
		return &jsonNode{kind: jsonNull}, nil
	default:
		return nil, p.syntaxError(path, nil)
	}
}

func (p *treeParser) object(path string, depth int) (*jsonNode, *FieldError) {
	n := &jsonNode{kind: jsonObject, obj: map[string]*jsonNode{}}
	for p.dec.More() {
		tok, err := p.dec.Token()
		if err != nil {
			return nil, p.syntaxError(path, err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, p.syntaxError(path, nil)
		}
		kp := joinKey(path, key)
		if _, dup := n.obj[key]; dup {
			return nil, &FieldError{Path: kp, Code: CodeDuplicateProperty, Message: "property appears more than once"}
		}
		child, ferr := p.value(kp, depth+1)
		if ferr != nil {
			return nil, ferr
		}
		n.keys = append(n.keys, key)
		n.obj[key] = child
	}
	if _, err := p.dec.Token(); err != nil { // '}'
		return nil, p.syntaxError(path, err)
	}
	return n, nil
}

func (p *treeParser) array(path string, depth int) (*jsonNode, *FieldError) {
	n := &jsonNode{kind: jsonArray, arr: []*jsonNode{}}
	for p.dec.More() {
		child, ferr := p.value(joinIndex(path, len(n.arr)), depth+1)
		if ferr != nil {
			return nil, ferr
		}
		n.arr = append(n.arr, child)
	}
	if _, err := p.dec.Token(); err != nil { // ']'
		return nil, p.syntaxError(path, err)
	}
	return n, nil
}
