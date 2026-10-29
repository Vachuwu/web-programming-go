package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"playlist/internal/domain"
	"playlist/internal/events"
	"playlist/internal/handler"
	"playlist/internal/repository"
	"playlist/internal/service"
)

var (
	testPool *pgxpool.Pool
	testURL  string
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:         "postgres:16-alpine",
			ExposedPorts:  []string{"5432/tcp"},
			Env:           map[string]string{"POSTGRES_USER": "playlist", "POSTGRES_PASSWORD": "playlist", "POSTGRES_DB": "playlist"},
			WaitingFor:    wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			AlwaysPullImage: false,
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start PostgreSQL test container:", err)
		os.Exit(1)
	}

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "get PostgreSQL host:", err)
		_ = container.Terminate(context.Background())
		os.Exit(1)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "get PostgreSQL port:", err)
		_ = container.Terminate(context.Background())
		os.Exit(1)
	}
	testURL = fmt.Sprintf("postgres://playlist:playlist@%s:%s/playlist?sslmode=disable", host, port.Port())
	testPool, err = pgxpool.New(ctx, testURL)
	if err == nil {
		err = applyMigration(ctx, testPool)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "prepare PostgreSQL schema:", err)
		_ = container.Terminate(context.Background())
		os.Exit(1)
	}

	code := m.Run()
	testPool.Close()
	_ = container.Terminate(context.Background())
	os.Exit(code)
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool) error {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("locate integration test source")
	}
	migration, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(sourceFile)), "migrations", "000001_init.up.sql"))
	if err != nil {
		return err
	}
	for _, statement := range strings.Split(string(migration), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

type testApp struct {
	server *httptest.Server
	hub    *events.Hub
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	resetDatabase(t)
	hub := events.NewHub()
	users := service.NewUserService(repository.NewUserRepository(testPool))
	playlists := service.NewPlaylistService(repository.NewPlaylistRepository(testPool))
	app := &testApp{hub: hub, server: httptest.NewServer(handler.NewWithEventHub(users, hub, playlists).Routes())}
	t.Cleanup(func() {
		app.server.Close()
		hub.Close()
	})
	return app
}

func resetDatabase(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := testPool.Exec(ctx, "TRUNCATE votes, playlist_items, users RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate test database: %v", err)
	}
	if _, err := testPool.Exec(ctx, "UPDATE playlist_state SET playback_version = 1 WHERE id = 1"); err != nil {
		t.Fatalf("reset playback version: %v", err)
	}
}

func (app *testApp) api(t *testing.T, method, path string, payload any, response any) int {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, app.server.URL+path, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := app.server.Client().Do(req)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer res.Body.Close()
	if response != nil {
		if err := json.NewDecoder(res.Body).Decode(response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return res.StatusCode
}

func (app *testApp) user(t *testing.T, name string) domain.User {
	t.Helper()
	var user domain.User
	if status := app.api(t, http.MethodPost, "/users", map[string]any{"name": name}, &user); status != http.StatusCreated {
		t.Fatalf("create user status = %d", status)
	}
	return user
}

func (app *testApp) item(t *testing.T, userID int64, title string) domain.PlaylistItem {
	t.Helper()
	var item domain.PlaylistItem
	if status := app.api(t, http.MethodPost, "/playlist/items", map[string]any{"userId": userID, "title": title, "artist": "Artist"}, &item); status != http.StatusCreated {
		t.Fatalf("add item status = %d", status)
	}
	return item
}

func (app *testApp) playlist(t *testing.T) domain.PlaylistSnapshot {
	t.Helper()
	var snapshot domain.PlaylistSnapshot
	if status := app.api(t, http.MethodGet, "/playlist", nil, &snapshot); status != http.StatusOK {
		t.Fatalf("read playlist status = %d", status)
	}
	return snapshot
}

func TestUsersAndItems(t *testing.T) {
	app := newTestApp(t)
	var errorBody map[string]any
	if status := app.api(t, http.MethodPost, "/users", map[string]any{"name": "   "}, &errorBody); status != http.StatusBadRequest {
		t.Fatalf("blank name status = %d, want 400", status)
	}
	user := app.user(t, "Ada")
	if user.ID == 0 || user.Name != "Ada" {
		t.Fatalf("unexpected user: %+v", user)
	}
	if status := app.api(t, http.MethodPost, "/playlist/items", map[string]any{"userId": 999, "title": "Unknown", "artist": "Artist"}, &errorBody); status != http.StatusNotFound {
		t.Fatalf("unknown user status = %d, want 404", status)
	}
	item := app.item(t, user.ID, "Track")
	playlist := app.playlist(t)
	if len(playlist.Queue) != 1 || playlist.Queue[0].ID != item.ID || playlist.Playing != nil {
		t.Fatalf("unexpected playlist: %+v", playlist)
	}
}

func TestQueueOrderingAndVotes(t *testing.T) {
	app := newTestApp(t)
	owner := app.user(t, "owner")
	voter := app.user(t, "voter")
	first := app.item(t, owner.ID, "first")
	second := app.item(t, owner.ID, "second")
	third := app.item(t, owner.ID, "third")

	var vote domain.VoteResult
	if status := app.api(t, http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", second.ID), map[string]any{"userId": voter.ID, "value": 1}, &vote); status != http.StatusOK || vote.Score != 1 {
		t.Fatalf("first vote status/result = %d/%+v", status, vote)
	}
	if status := app.api(t, http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", third.ID), map[string]any{"userId": voter.ID, "value": 1}, &vote); status != http.StatusOK {
		t.Fatalf("second vote status = %d", status)
	}
	playlist := app.playlist(t)
	if got := []int64{playlist.Queue[0].ID, playlist.Queue[1].ID, playlist.Queue[2].ID}; got[0] != second.ID || got[1] != third.ID || got[2] != first.ID {
		t.Fatalf("queue order = %v", got)
	}
	if status := app.api(t, http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", second.ID), map[string]any{"userId": voter.ID, "value": -1}, &vote); status != http.StatusOK || vote.Score != -1 {
		t.Fatalf("changed vote status/result = %d/%+v", status, vote)
	}
	if status := app.api(t, http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", third.ID), map[string]any{"userId": voter.ID, "value": 1}, &vote); status != http.StatusOK || vote.Score != 1 {
		t.Fatalf("repeat vote status/result = %d/%+v", status, vote)
	}
	if status := app.api(t, http.MethodDelete, fmt.Sprintf("/playlist/items/%d/vote?userId=%d", third.ID, voter.ID), nil, &vote); status != http.StatusOK || vote.Score != 0 {
		t.Fatalf("delete vote status/result = %d/%+v", status, vote)
	}
	if status := app.api(t, http.MethodDelete, fmt.Sprintf("/playlist/items/%d/vote?userId=%d", third.ID, voter.ID), nil, &vote); status != http.StatusOK || vote.Score != 0 {
		t.Fatalf("repeat delete status/result = %d/%+v", status, vote)
	}
}

func TestConcurrentVotes(t *testing.T) {
	t.Run("different users", func(t *testing.T) {
		app := newTestApp(t)
		owner := app.user(t, "owner")
		item := app.item(t, owner.ID, "track")
		users := make([]domain.User, 100)
		for i := range users {
			users[i] = app.user(t, fmt.Sprintf("user-%d", i))
		}
		runConcurrentVotes(t, app, item.ID, users, 1)
		if score := app.playlist(t).Queue[0].Score; score != 100 {
			t.Fatalf("score = %d, want 100", score)
		}
	})
	t.Run("same user", func(t *testing.T) {
		app := newTestApp(t)
		owner := app.user(t, "owner")
		voter := app.user(t, "voter")
		item := app.item(t, owner.ID, "track")
		users := make([]domain.User, 20)
		for i := range users {
			users[i] = voter
		}
		runConcurrentVotes(t, app, item.ID, users, 1)
		if score := app.playlist(t).Queue[0].Score; score != 1 {
			t.Fatalf("score = %d, want 1", score)
		}
		var count int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM votes WHERE item_id = $1", item.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("vote rows = %d, want 1", count)
		}
	})
}

func runConcurrentVotes(t *testing.T, app *testApp, itemID int64, users []domain.User, value int16) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, len(users))
	var group sync.WaitGroup
	for _, user := range users {
		group.Add(1)
		go func(userID int64) {
			defer group.Done()
			<-start
			var result domain.VoteResult
			if status := app.apiNoFail(http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", itemID), map[string]any{"userId": userID, "value": value}, &result); status != http.StatusOK {
				errs <- fmt.Errorf("vote status = %d", status)
			}
		}(user.ID)
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func (app *testApp) apiNoFail(method, path string, payload any, response any) int {
	encoded, _ := json.Marshal(payload)
	req, _ := http.NewRequest(method, app.server.URL+path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.server.Client().Do(req)
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	_ = json.NewDecoder(res.Body).Decode(response)
	return res.StatusCode
}

func TestPlaybackAndConcurrentNext(t *testing.T) {
	app := newTestApp(t)
	owner := app.user(t, "owner")
	first := app.item(t, owner.ID, "first")
	second := app.item(t, owner.ID, "second")
	var snapshot domain.PlaylistSnapshot
	if status := app.api(t, http.MethodPost, "/playlist/play", map[string]any{"expectedVersion": 1}, &snapshot); status != http.StatusOK || snapshot.Playing == nil || snapshot.Playing.ID != first.ID {
		t.Fatalf("play status/snapshot = %d/%+v", status, snapshot)
	}
	if status := app.api(t, http.MethodPost, "/playlist/play", map[string]any{"expectedVersion": snapshot.PlaybackVersion}, &snapshot); status != http.StatusConflict {
		t.Fatalf("repeat play status = %d, want 409", status)
	}
	start := make(chan struct{})
	statuses := make(chan int, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			statuses <- app.apiNoFail(http.MethodPost, "/playlist/next", map[string]any{"expectedVersion": 2}, &domain.PlaylistSnapshot{})
		}()
	}
	close(start)
	group.Wait()
	close(statuses)
	var success, conflict int
	for status := range statuses {
		if status == http.StatusOK {
			success++
		}
		if status == http.StatusConflict {
			conflict++
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("next statuses: %d successes, %d conflicts", success, conflict)
	}
	snapshot = app.playlist(t)
	if snapshot.Playing == nil || snapshot.Playing.ID != second.ID || snapshot.PlaybackVersion != 3 {
		t.Fatalf("concurrent next skipped an item: %+v", snapshot)
	}
	if status := app.api(t, http.MethodPost, "/playlist/next", map[string]any{"expectedVersion": 3}, &snapshot); status != http.StatusOK || snapshot.Playing != nil {
		t.Fatalf("last next status/snapshot = %d/%+v", status, snapshot)
	}
	if status := app.api(t, http.MethodPost, "/playlist/next", map[string]any{"expectedVersion": 4}, &snapshot); status != http.StatusConflict {
		t.Fatalf("next without playing status = %d, want 409", status)
	}
}

func TestRemovalHistoryAndRestart(t *testing.T) {
	app := newTestApp(t)
	owner := app.user(t, "owner")
	other := app.user(t, "other")
	queued := app.item(t, owner.ID, "queued")
	if status := app.api(t, http.MethodDelete, fmt.Sprintf("/playlist/items/%d", queued.ID), map[string]any{"userId": other.ID}, &map[string]any{}); status != http.StatusForbidden {
		t.Fatalf("non-author removal status = %d", status)
	}
	if status := app.api(t, http.MethodDelete, fmt.Sprintf("/playlist/items/%d", queued.ID), map[string]any{"userId": owner.ID}, &domain.PlaylistItem{}); status != http.StatusOK {
		t.Fatalf("author removal status = %d", status)
	}
	playing := app.item(t, owner.ID, "playing")
	var snapshot domain.PlaylistSnapshot
	if status := app.api(t, http.MethodPost, "/playlist/play", map[string]any{"expectedVersion": 1}, &snapshot); status != http.StatusOK {
		t.Fatalf("play status = %d", status)
	}
	if status := app.api(t, http.MethodDelete, fmt.Sprintf("/playlist/items/%d", playing.ID), map[string]any{"userId": owner.ID}, &map[string]any{}); status != http.StatusConflict {
		t.Fatalf("remove playing status = %d", status)
	}
	if status := app.api(t, http.MethodPost, "/playlist/next", map[string]any{"expectedVersion": 2}, &snapshot); status != http.StatusOK {
		t.Fatalf("finish playing status = %d", status)
	}
	if status := app.api(t, http.MethodDelete, fmt.Sprintf("/playlist/items/%d", playing.ID), map[string]any{"userId": owner.ID}, &map[string]any{}); status != http.StatusConflict {
		t.Fatalf("remove played status = %d", status)
	}
	app.server.Close()
	app.server = httptest.NewServer(handler.NewWithEventHub(service.NewUserService(repository.NewUserRepository(testPool)), events.NewHub(), service.NewPlaylistService(repository.NewPlaylistRepository(testPool))).Routes())
	var history []domain.PlaylistItem
	if status := app.api(t, http.MethodGet, "/playlist/history", nil, &history); status != http.StatusOK || len(history) != 1 || history[0].ID != playing.ID {
		t.Fatalf("history after restart = %d/%+v", status, history)
	}
}

func TestConflictingOperationsLeaveValidState(t *testing.T) {
	for _, scenario := range []string{"vote-next", "add-next", "remove-next"} {
		t.Run(scenario, func(t *testing.T) {
			app := newTestApp(t)
			owner := app.user(t, "owner")
			voter := app.user(t, "voter")
			first := app.item(t, owner.ID, "first")
			second := app.item(t, owner.ID, "second")
			var snapshot domain.PlaylistSnapshot
			if status := app.api(t, http.MethodPost, "/playlist/play", map[string]any{"expectedVersion": 1}, &snapshot); status != http.StatusOK {
				t.Fatalf("play status = %d", status)
			}
			start := make(chan struct{})
			var group sync.WaitGroup
			group.Add(2)
			go func() {
				defer group.Done()
				<-start
				switch scenario {
				case "vote-next":
					app.apiNoFail(http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", second.ID), map[string]any{"userId": voter.ID, "value": 1}, &domain.VoteResult{})
				case "add-next":
					app.apiNoFail(http.MethodPost, "/playlist/items", map[string]any{"userId": owner.ID, "title": "third", "artist": "Artist"}, &domain.PlaylistItem{})
				case "remove-next":
					app.apiNoFail(http.MethodDelete, fmt.Sprintf("/playlist/items/%d", second.ID), map[string]any{"userId": owner.ID}, &domain.PlaylistItem{})
				}
			}()
			go func() {
				defer group.Done()
				<-start
				app.apiNoFail(http.MethodPost, "/playlist/next", map[string]any{"expectedVersion": 2}, &domain.PlaylistSnapshot{})
			}()
			close(start)
			group.Wait()
			var playingCount int
			if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM playlist_items WHERE status = 'playing'").Scan(&playingCount); err != nil {
				t.Fatal(err)
			}
			if playingCount > 1 {
				t.Fatalf("%s produced %d playing items (first id %d)", scenario, playingCount, first.ID)
			}
		})
	}
}

func TestSSEPublishesOnlyCommittedChanges(t *testing.T) {
	app := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, app.server.URL+"/playlist/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := app.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", res.StatusCode)
	}
	eventsReceived := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				eventsReceived <- strings.TrimPrefix(line, "event: ")
			}
		}
		close(eventsReceived)
	}()
	owner := app.user(t, "owner")
	select {
	case name := <-eventsReceived:
		if name != "item_added" {
			t.Fatalf("event = %q, want item_added", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no SSE event after committed item")
	}
	var result domain.VoteResult
	item := app.item(t, owner.ID, "track")
	<-eventsReceived
	if status := app.api(t, http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", item.ID), map[string]any{"userId": owner.ID, "value": 1}, &result); status != http.StatusOK {
		t.Fatalf("vote status = %d", status)
	}
	<-eventsReceived
	if status := app.api(t, http.MethodPost, fmt.Sprintf("/playlist/items/%d/vote", item.ID), map[string]any{"userId": owner.ID, "value": 1}, &result); status != http.StatusOK {
		t.Fatalf("repeat vote status = %d", status)
	}
	select {
	case unexpected := <-eventsReceived:
		t.Fatalf("unexpected no-op event %q", unexpected)
	case <-time.After(200 * time.Millisecond):
	}
	res.Body.Close()
	if status := app.api(t, http.MethodPost, "/playlist/items", map[string]any{"userId": 999, "title": "rollback", "artist": "Artist"}, &map[string]any{}); status != http.StatusNotFound {
		t.Fatalf("rollback request status = %d", status)
	}
}
