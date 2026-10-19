// This private-fixture harness compares the real source reader and HTTP client.
// Run via ../replay_sample.py; it never prints transcript bodies or identifiers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

type diskObjects map[string]string

func (d diskObjects) Read(_ context.Context, key string) ([]byte, error) { return os.ReadFile(d[key]) }
func (d diskObjects) Put(context.Context, string, []byte, string) error {
	return fmt.Errorf("read-only fixture")
}
func (d diskObjects) Delete(context.Context, string) error { return fmt.Errorf("read-only fixture") }
func (d diskObjects) Location() string                     { return "sample" }

type session struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id"`
	Generation       int    `json:"generation"`
	LastSeq          int    `json:"last_seq"`
	MergedKey        string `json:"merged_key"`
	MergedThroughSeq int    `json:"merged_through_seq"`
	Turns            []struct {
		Seq        int    `json:"seq"`
		RequestID  string `json:"request_id"`
		SegmentKey string `json:"segment_key"`
	} `json:"turns"`
}

func statistics(values []float64) map[string]float64 {
	sort.Float64s(values)
	return map[string]float64{"median_ms": values[len(values)/2], "p95_ms": values[(len(values)*95-1)/100]}
}

func main() {
	if len(os.Args) != 3 {
		panic("requires sample directory and service URL")
	}
	data, err := os.ReadFile(filepath.Join(os.Args[1], "selection.json"))
	if err != nil {
		panic(err)
	}
	var selection struct {
		Sessions []session `json:"sessions"`
		Objects  []struct {
			Key   string `json:"key"`
			Index int    `json:"object_index"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(data, &selection); err != nil {
		panic(err)
	}
	objects := diskObjects{}
	for _, object := range selection.Objects {
		objects[object.Key] = filepath.Join(os.Args[1], "objects", fmt.Sprint(object.Index)+".gz")
	}
	reader := sessionarchive.NewReader(objects)
	client := sessionarchive.NewQueryClient(os.Args[2], os.Getenv("SESSION_QUERY_REPLAY_TOKEN"), 5*time.Second)
	var sourceTimes, queryTimes []float64
	pages, crossings := 0, 0
	for _, s := range selection.Sessions {
		state := &sessionarchive.State{TranscriptID: s.ID, TenantID: s.TenantID, Generation: s.Generation, LastSeq: s.LastSeq, MergedKey: s.MergedKey, MergedThroughSeq: s.MergedThroughSeq}
		var turns []sessionarchive.Turn
		for _, turn := range s.Turns {
			turns = append(turns, sessionarchive.Turn{Generation: s.Generation, Seq: turn.Seq, RequestID: turn.RequestID, SegmentKey: turn.SegmentKey})
		}
		starts := map[int]bool{1: true, max(1, s.LastSeq/2): true, max(1, s.LastSeq-19): true, s.LastSeq: true, s.LastSeq + 1: true}
		for _, seq := range []int{127, 128, 129, 255, 256, 257} {
			if seq <= s.LastSeq {
				starts[seq] = true
			}
		}
		var ordered []int
		for seq := range starts {
			ordered = append(ordered, seq)
		}
		sort.Ints(ordered)
		for _, start := range ordered {
			for repetition := 0; repetition < 3; repetition++ {
				timer := time.Now()
				expected, err := reader.Page(context.Background(), state, turns, start, 20)
				sourceTimes = append(sourceTimes, float64(time.Since(timer))/float64(time.Millisecond))
				if err != nil {
					panic(err)
				}
				timer = time.Now()
				actual, err := client.Page(context.Background(), state, start, 20)
				queryTimes = append(queryTimes, float64(time.Since(timer))/float64(time.Millisecond))
				if err != nil {
					panic(err)
				}
				if !reflect.DeepEqual(expected, actual) {
					panic(fmt.Sprintf("render mismatch at sample page %d", pages))
				}
				pages++
				if (max(1, start-1)-1)/128 != (min(s.LastSeq, start+19)-1)/128 && start <= s.LastSeq {
					crossings++
				}
			}
		}
	}
	result := map[string]any{"display_page_comparisons": pages, "chunk_boundary_comparisons": crossings, "render_mismatches": 0, "source_local": statistics(sourceTimes), "service_local_http": statistics(queryTimes)}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(err)
	}
}
