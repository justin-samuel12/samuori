package buildspec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"go.yaml.in/yaml/v3"
)

// Canonical renders the document as block-style YAML in schema field order, with x-* extensions last.
func Canonical(d *Document) ([]byte, error) {
	js, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(js, &node); err != nil {
		return nil, err
	}
	blockStyle(&node)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Digest identifies a revision by content, independent of the YAML formatting it was written in.
func Digest(d *Document) (string, error) {
	js, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(js)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func blockStyle(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		n.Style = 0
	} else {
		n.Style &^= yaml.FlowStyle
	}
	for _, c := range n.Content {
		blockStyle(c)
	}
}
