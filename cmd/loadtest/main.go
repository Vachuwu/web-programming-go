// Command loadtest prepares data through the public API and measures API load.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type state struct {
	Users []int64 `json:"users"`
	Items []int64 `json:"items"`
}

type playlist struct {
	PlaybackVersion int64           `json:"playbackVersion"`
	Playing         json.RawMessage `json:"playing"`
}

type result struct {
	Status           int
	Duration         time.Duration
	Err              error
	ExpectedConflict bool
}

type summary struct {
	Total       int
	Statuses    map[int]int
	Durations   []time.Duration
	Transport   int
	Expected409 int
	Technical   int
	Elapsed     time.Duration
}

func main() {
	mode := flag.String("mode", "run", "prepare or run")
	scenario := flag.String("scenario", "vote", "get, vote, next or mixed")
	baseURL := flag.String("base-url", "http://localhost:8080", "server base URL")
	workers := flag.Int("workers", 100, "number of concurrent workers")
	requests := flag.Int("requests", 10000, "number of measured requests")
	users := flag.Int("users", 100, "users to create during prepare")
	items := flag.Int("items", 1000, "queued items to create during prepare")
	votes := flag.Int("votes", 10000, "votes to create during prepare")
	stateFile := flag.String("state", "loadtest-state.json", "file with prepared resource IDs")
	flag.Parse()

	if *workers < 1 || *requests < 1 || *users < 1 || *items < 1 || *votes < 0 {
		fatal("workers, requests, users and items must be positive; votes cannot be negative")
	}
	*baseURL = strings.TrimRight(*baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		MaxIdleConns:        *workers * 2,
		MaxIdleConnsPerHost: *workers * 2,
		IdleConnTimeout:     90 * time.Second,
	}}

	switch *mode {
	case "prepare":
		prepared, err := prepare(client, *baseURL, *users, *items, *votes)
		if err != nil {
			fatal("prepare failed: %v", err)
		}
		if err := writeState(*stateFile, prepared); err != nil {
			fatal("save state: %v", err)
		}
		fmt.Printf("Prepared %d users and %d items through REST API. State: %s\n", len(prepared.Users), len(prepared.Items), *stateFile)
	case "run":
		prepared, err := readState(*stateFile)
		if err != nil {
			fatal("read prepared state: %v (run -mode=prepare first)", err)
		}
		if err := validateScenario(*scenario, prepared); err != nil {
			fatal("invalid scenario: %v", err)
		}
		measurement, err := run(client, *baseURL, *scenario, prepared, *workers, *requests)
		if err != nil {
			fatal("run failed: %v", err)
		}
		printSummary(*scenario, *workers, measurement)
	default:
		fatal("unknown mode %q; use prepare or run", *mode)
	}
}

// prepare intentionally uses only the public REST API. Its duration is never
// included in the measured run.
func prepare(client *http.Client, baseURL string, userCount, itemCount, voteCount int) (state, error) {
	prepared := state{Users: make([]int64, 0, userCount), Items: make([]int64, 0, itemCount)}
	suffix := time.Now().UTC().Format("20060102T150405.000000000")
	for i := 0; i < userCount; i++ {
		var response struct {
			ID int64 `json:"id"`
		}
		if err := postJSON(client, baseURL+"/users", map[string]any{"name": fmt.Sprintf("loadtest-%s-user-%d", suffix, i)}, &response); err != nil {
			return state{}, fmt.Errorf("create user %d: %w", i, err)
		}
		prepared.Users = append(prepared.Users, response.ID)
	}
	for i := 0; i < itemCount; i++ {
		var response struct {
			ID int64 `json:"id"`
		}
		body := map[string]any{"userId": prepared.Users[i%len(prepared.Users)], "title": fmt.Sprintf("loadtest track %d", i), "artist": "loadtest"}
		if err := postJSON(client, baseURL+"/playlist/items", body, &response); err != nil {
			return state{}, fmt.Errorf("create item %d: %w", i, err)
		}
		prepared.Items = append(prepared.Items, response.ID)
	}
	for i := 0; i < voteCount; i++ {
		itemID := prepared.Items[i%len(prepared.Items)]
		userID := prepared.Users[(i/len(prepared.Items))%len(prepared.Users)]
		if err := postJSON(client, fmt.Sprintf("%s/playlist/items/%d/vote", baseURL, itemID), map[string]any{"userId": userID, "value": 1}, nil); err != nil {
			return state{}, fmt.Errorf("prepare vote %d: %w", i, err)
		}
	}
	return prepared, nil
}

func run(client *http.Client, baseURL, scenario string, prepared state, workers, requests int) (summary, error) {
	current, err := currentPlaylist(client, baseURL)
	if err != nil {
		return summary{}, err
	}
	version := current.PlaybackVersion
	if (scenario == "next" || scenario == "mixed") && (len(current.Playing) == 0 || string(current.Playing) == "null") {
		if err := postJSON(client, baseURL+"/playlist/play", map[string]any{"expectedVersion": version}, &current); err != nil {
			return summary{}, fmt.Errorf("start playback for %s scenario: %w", scenario, err)
		}
		version = current.PlaybackVersion
	}
	tasks := make(chan int)
	results := make(chan result, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(seed int64) {
			defer group.Done()
			random := rand.New(rand.NewSource(seed))
			for task := range tasks {
				results <- execute(client, baseURL, scenario, prepared, version, task, random)
			}
		}(time.Now().UnixNano() + int64(worker))
	}
	go func() {
		for task := 0; task < requests; task++ {
			tasks <- task
		}
		close(tasks)
		group.Wait()
		close(results)
	}()

	started := time.Now()
	measured := summary{Statuses: make(map[int]int), Durations: make([]time.Duration, 0, requests)}
	for result := range results {
		measured.Total++
		measured.Durations = append(measured.Durations, result.Duration)
		if result.Err != nil {
			measured.Transport++
			continue
		}
		measured.Statuses[result.Status]++
		if result.ExpectedConflict {
			measured.Expected409++
		} else if result.Status < 200 || result.Status >= 300 {
			measured.Technical++
		}
	}
	measured.Elapsed = time.Since(started)
	return measured, nil
}

func execute(client *http.Client, baseURL, scenario string, prepared state, version int64, task int, random *rand.Rand) result {
	operation := scenario
	if scenario == "mixed" {
		switch random.Intn(10) {
		case 0, 1:
			operation = "next"
		case 2, 3, 4:
			operation = "vote"
		default:
			operation = "get"
		}
	}
	started := time.Now()
	var request *http.Request
	var err error
	switch operation {
	case "get":
		request, err = http.NewRequest(http.MethodGet, baseURL+"/playlist", nil)
	case "vote":
		itemID := prepared.Items[task%len(prepared.Items)]
		userID := prepared.Users[(task/len(prepared.Items))%len(prepared.Users)]
		// Preparation uses +1; a -1 in the measured run makes this a real vote
		// change instead of repeatedly measuring the no-op path.
		request, err = jsonRequest(http.MethodPost, fmt.Sprintf("%s/playlist/items/%d/vote", baseURL, itemID), map[string]any{"userId": userID, "value": -1})
	case "next":
		request, err = jsonRequest(http.MethodPost, baseURL+"/playlist/next", map[string]any{"expectedVersion": version})
	default:
		return result{Err: fmt.Errorf("unknown operation %q", operation)}
	}
	if err != nil {
		return result{Duration: time.Since(started), Err: err}
	}
	response, err := client.Do(request)
	if err != nil {
		return result{Duration: time.Since(started), Err: err}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return result{
		Status:           response.StatusCode,
		Duration:         time.Since(started),
		ExpectedConflict: operation == "next" && response.StatusCode == http.StatusConflict,
	}
}

func currentPlaylist(client *http.Client, baseURL string) (playlist, error) {
	response, err := client.Get(baseURL + "/playlist")
	if err != nil {
		return playlist{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return playlist{}, fmt.Errorf("GET /playlist: status %d", response.StatusCode)
	}
	var current playlist
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		return playlist{}, err
	}
	return current, nil
}

func postJSON(client *http.Client, url string, body any, destination any) error {
	request, err := jsonRequest(http.MethodPost, url, body)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if destination != nil {
		return json.NewDecoder(response.Body).Decode(destination)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func jsonRequest(method, url string, body any) (*http.Request, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(encoded))
	if err == nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, err
}

func validateScenario(scenario string, prepared state) error {
	if len(prepared.Users) == 0 || len(prepared.Items) == 0 {
		return errors.New("prepared state has no users or items")
	}
	switch scenario {
	case "get", "vote", "next", "mixed":
		return nil
	}
	return fmt.Errorf("unknown scenario %q", scenario)
}

func writeState(path string, prepared state) error {
	data, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func readState(path string) (state, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return state{}, err
	}
	var prepared state
	if err := json.Unmarshal(data, &prepared); err != nil {
		return state{}, err
	}
	return prepared, nil
}

func printSummary(scenario string, workers int, measured summary) {
	if len(measured.Durations) == 0 {
		fatal("no results")
	}
	sort.Slice(measured.Durations, func(i, j int) bool { return measured.Durations[i] < measured.Durations[j] })
	var total time.Duration
	for _, duration := range measured.Durations {
		total += duration
	}
	successes := 0
	for status, count := range measured.Statuses {
		if status >= 200 && status < 300 {
			successes += count
		}
	}
	technical := measured.Transport + measured.Technical
	rps := float64(measured.Total) / math.Max(measured.Elapsed.Seconds(), 0.001)
	fmt.Printf("scenario=%s workers=%d total=%d success=%d technical_errors=%d expected_409=%d\n", scenario, workers, measured.Total, successes, technical, measured.Expected409)
	fmt.Printf("statuses=%v\n", measured.Statuses)
	fmt.Printf("rps=%.2f avg=%s p50=%s p95=%s p99=%s\n", rps, total/time.Duration(measured.Total), percentile(measured.Durations, .50), percentile(measured.Durations, .95), percentile(measured.Durations, .99))
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	position := int(math.Ceil(float64(len(values))*fraction)) - 1
	if position < 0 {
		position = 0
	}
	return values[position]
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
