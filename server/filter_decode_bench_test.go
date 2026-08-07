package server

// Request-decode cost, unfiltered vs filtered: the duplicate-aware token
// walk runs on every queryReq decode (it cannot be gated on the typed
// map, because {"filter":{...},"filter":{}} last-wins to an EMPTY map),
// so its cost on an ORDINARY body is exactly the unfiltered HTTP
// overhead the perf gate cares about. Run with -bench QueryReqDecode.

import (
	"encoding/json"
	"testing"
)

var decodeBodies = map[string][]byte{
	"plain":    []byte(`{"q":"anna hazare","lists":["leie","sdn","un","csl","eu"]}`),
	"opts":     []byte(`{"q":"anna hazare","lists":["leie","sdn","un","csl","eu"],"threshold":0.6,"topk":100}`),
	"filtered": []byte(`{"q":"anna hazare","lists":["sanctions"],"filter":{"program":"SDN","country":"EE"}}`),
}

func BenchmarkQueryReqDecode(b *testing.B) {
	for name, body := range decodeBodies {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var q queryReq
				if err := json.Unmarshal(body, &q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// The walk alone, isolated from the typed decode.
func BenchmarkDupFilterWalk(b *testing.B) {
	for name, body := range decodeBodies {
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := dupFilterName(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
