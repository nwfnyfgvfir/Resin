package subscription

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	sJson "github.com/sagernet/sing/common/json"
)

// Keep ordinary JSON on the existing parse path. Only filter comments when
// the opening token or a reported syntax error points to a comment.
func parseSubscriptionJSON(data []byte) (nodes []ParsedNode, recognized bool, err error) {
	var syntax *json.SyntaxError
	if looksLikeJSON(data) {
		nodes, recognized, err = parseJSONSubscription(data)
		if !errors.As(err, &syntax) || syntax.Offset <= 0 || syntax.Offset > int64(len(data)) || !startsJSONComment(data[syntax.Offset-1:]) {
			return nodes, recognized, err
		}
	} else {
		leading := data
		if data[0] == '[' {
			leading = bytes.TrimSpace(data[1:])
		}
		if !startsJSONComment(leading) {
			return nil, false, nil
		}
	}
	filtered, filterErr := io.ReadAll(sJson.NewCommentFilter(bytes.NewReader(data)))
	filtered = bytes.TrimSpace(filtered)
	if filterErr != nil || !looksLikeJSON(filtered) {
		return nodes, recognized, err
	}
	filteredNodes, filteredRecognized, filteredErr := parseJSONSubscription(filtered)
	if errors.As(filteredErr, &syntax) {
		return nodes, recognized, err
	}
	return filteredNodes, filteredRecognized, filteredErr
}

func startsJSONComment(data []byte) bool {
	return len(data) >= 2 && data[0] == '/' && (data[1] == '/' || data[1] == '*')
}

func parseSingboxEndpoints(raw json.RawMessage) ([]ParsedNode, error) {
	var endpoints []json.RawMessage
	if err := json.Unmarshal(raw, &endpoints); err != nil {
		return nil, fmt.Errorf("subscription: unmarshal endpoints: %w", err)
	}
	nodes := make([]ParsedNode, 0, len(endpoints))
	for _, entry := range endpoints {
		var header struct {
			outboundHeader
			Address json.RawMessage `json:"address"`
		}
		if err := json.Unmarshal(entry, &header); err != nil || header.Type != "wireguard" || len(header.Address) == 0 {
			continue
		}
		nodes = append(nodes, ParsedNode{Tag: header.Tag, RawOptions: entry})
	}
	return nodes, nil
}

func appendUnwrappedJSONNodes(nodes []ParsedNode, raw json.RawMessage) ([]ParsedNode, bool) {
	var header struct {
		outboundHeader
		Port       json.RawMessage `json:"port"`
		ServerPort json.RawMessage `json:"server_port"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nodes, false
	}
	if header.Type == "" && header.Protocol != "" {
		return appendXrayOutbounds(nodes, raw), true
	}
	if len(header.Port) > 0 && len(header.ServerPort) == 0 {
		var proxy map[string]any
		if err := json.Unmarshal(raw, &proxy); err != nil {
			return nodes, false
		}
		if node, ok := convertClashProxyToNode(proxy); ok {
			nodes = append(nodes, node)
		}
		return nodes, false
	}
	if !supportedOutboundTypes[header.Type] {
		return nodes, false
	}
	return append(nodes, ParsedNode{Tag: header.Tag, RawOptions: append(json.RawMessage(nil), raw...)}), false
}

type clashProxyProvider struct {
	Type    string           `json:"type" yaml:"type"`
	Payload []map[string]any `json:"payload" yaml:"payload"`
}

func appendInlineClashProxies(nodes []ParsedNode, providers map[string]clashProxyProvider) []ParsedNode {
	names := make([]string, 0, len(providers))
	for name, provider := range providers {
		if strings.EqualFold(provider.Type, "inline") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		for _, proxy := range providers[name].Payload {
			if node, ok := convertClashProxyToNode(proxy); ok {
				nodes = append(nodes, node)
			}
		}
	}
	return nodes
}
