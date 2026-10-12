package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/httpapi"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

// commitCutProxy forwards PostgreSQL protocol messages over a real TCP connection.
// It cuts exactly one transaction either before forwarding COMMIT, or after the
// server's COMMIT CommandComplete but before forwarding that acknowledgment.
// This is a test-only proxy for the disposable local, non-TLS database fixture.
func commitCutProxy(t *testing.T, upstream string, afterCommit bool) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var enabled atomic.Bool
	enabled.Store(true)
	hit := make(chan string, 1)
	var connections sync.Map
	var workers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(client, true)
			workers.Add(1)
			go func(client net.Conn) {
				defer workers.Done()
				defer client.Close()
				defer connections.Delete(client)
				server, err := net.DialTimeout("tcp", upstream, 2*time.Second)
				if err != nil {
					return
				}
				connections.Store(server, true)
				defer connections.Delete(server)
				defer server.Close()
				// StartupMessage has no leading type byte.
				header := make([]byte, 4)
				if _, err = io.ReadFull(client, header); err != nil {
					return
				}
				n := binary.BigEndian.Uint32(header)
				if n < 8 || n > 1<<20 {
					return
				}
				body := make([]byte, int(n)-4)
				if _, err = io.ReadFull(client, body); err != nil {
					return
				}
				if _, err = io.Copy(server, bytes.NewReader(append(header, body...))); err != nil {
					return
				}
				var committing atomic.Bool
				relayDone := make(chan struct{})
				go func() {
					defer close(relayDone)
					defer client.Close()
					defer server.Close()
					for {
						packet, err := pgTestPacket(server)
						if err != nil {
							return
						}
						if afterCommit && committing.Load() && packet[0] == 'C' && string(packet[5:]) == "COMMIT\x00" && enabled.CompareAndSwap(true, false) {
							hit <- "server committed; acknowledgment dropped"
							return
						}
						if _, err = io.Copy(client, bytes.NewReader(packet)); err != nil {
							return
						}
					}
				}()
				defer func() { client.Close(); server.Close(); <-relayDone }()
				for {
					packet, err := pgTestPacket(client)
					if err != nil {
						return
					}
					if packet[0] == 'Q' && strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(string(packet[5:]), "\x00")), "commit") {
						committing.Store(true)
						if !afterCommit && enabled.CompareAndSwap(true, false) {
							hit <- "connection cut before COMMIT reached server"
							return
						}
					}
					if _, err = io.Copy(server, bytes.NewReader(packet)); err != nil {
						return
					}
				}
			}(client)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-accepted
		connections.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	return listener.Addr().(*net.TCPAddr).Port, hit
}

func pgTestPacket(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n < 4 || n > 1<<20 {
		return nil, fmt.Errorf("invalid PostgreSQL test frame size %d", n)
	}
	body := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return append(header, body...), nil
}

func TestDatabaseCommitDisconnect(t *testing.T) {
	for _, after := range []bool{false, true} {
		for _, operation := range []string{"POST", "PUT", "DELETE"} {
			t.Run(fmt.Sprintf("%s/after_commit_%t", operation, after), func(t *testing.T) {
				f := setup(t)
				body := `{"name":"original","status":0}`
				var id int64
				if operation != "POST" {
					v := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", body, 201), "original", 0)
					id = v.ID
				}
				config := f.pool.Config()
				port, hit := commitCutProxy(t, net.JoinHostPort(config.ConnConfig.Host, fmt.Sprint(config.ConnConfig.Port)), after)
				config.ConnConfig.Host = "127.0.0.1"
				config.ConnConfig.Port = uint16(port)
				pool, err := pgxpool.NewWithConfig(t.Context(), config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				repo := postgres.New(pool)
				server := httptest.NewServer(httpapi.New(task.NewService(repo), repo, false, pool.Ping))
				t.Cleanup(server.Close)
				proxied := *f
				proxied.pool = pool
				proxied.server = server
				path := fmt.Sprintf("/tasks/%d", id)
				if operation == "POST" {
					code, _, err := keyedPost(server.URL, f.a, "101", "cut-commit", body)
					if err != nil || code != 503 {
						t.Fatalf("cut POST should report unavailable: %d %v", code, err)
					}
				} else {
					payload := ""
					if operation == "PUT" {
						payload = `{"name":"changed","status":1,"version":1}`
					}
					proxied.call(t, operation, path, f.a, "101", payload, 503)
				}
				select {
				case boundary := <-hit:
					t.Log(boundary)
				case <-time.After(time.Second):
					t.Fatal("requested protocol boundary was not cut")
				}
				ctx := context.Background()
				if operation == "POST" {
					var tasks, keys int
					if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE tenant_id=101),(SELECT count(*) FROM task_create_keys WHERE tenant_id=101 AND user_id=1001)`).Scan(&tasks, &keys); err != nil {
						t.Fatal(err)
					}
					want := 0
					if after {
						want = 1
					}
					if tasks != want || keys != want {
						t.Fatalf("before retry tasks=%d keys=%d want=%d", tasks, keys, want)
					}
					var committedID int64
					if after {
						if err = f.pool.QueryRow(ctx, `SELECT id FROM tasks WHERE tenant_id=101`).Scan(&committedID); err != nil {
							t.Fatal(err)
						}
					}
					code, v, err := keyedPost(server.URL, f.a, "101", "cut-commit", body)
					if err != nil || code != 201 || v.Version != 1 || (after && v.ID != committedID) {
						t.Fatalf("retry %d %+v %v prior=%d", code, v, err, committedID)
					}
					if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM tasks t JOIN task_create_keys k ON k.tenant_id=t.tenant_id AND k.task_id=t.id WHERE t.tenant_id=101`).Scan(&tasks); err != nil || tasks != 1 {
						t.Fatalf("retry must leave one task/key pair: %d %v", tasks, err)
					}
				} else if operation == "PUT" {
					var version int64
					var name string
					var status int
					if err = f.pool.QueryRow(ctx, `SELECT name,status,version FROM tasks WHERE tenant_id=101 AND id=$1`, id).Scan(&name, &status, &version); err != nil {
						t.Fatal(err)
					}
					if after {
						if name != "changed" || status != 1 || version != 2 {
							t.Fatalf("committed update missing: %s %d %d", name, status, version)
						}
					} else if name != "original" || status != 0 || version != 1 {
						t.Fatalf("uncommitted update leaked: %s %d %d", name, status, version)
					}
					want := 200
					if after {
						want = 409
					}
					proxied.call(t, "PUT", path, f.a, "101", `{"name":"changed","status":1,"version":1}`, want)
					if err = f.pool.QueryRow(ctx, `SELECT version FROM tasks WHERE tenant_id=101 AND id=$1`, id).Scan(&version); err != nil || version != 2 {
						t.Fatalf("retry advanced version twice: %d %v", version, err)
					}
				} else {
					var count int
					if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant_id=101 AND id=$1`, id).Scan(&count); err != nil {
						t.Fatal(err)
					}
					want := 1
					if after {
						want = 0
					}
					if count != want {
						t.Fatalf("delete postcondition %d want=%d", count, want)
					}
					code := 204
					if after {
						code = 404
					}
					proxied.call(t, "DELETE", path, f.a, "101", "", code)
					if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant_id=101 AND id=$1`, id).Scan(&count); err != nil || count != 0 {
						t.Fatalf("delete retry failed %d %v", count, err)
					}
				}
			})
		}
	}
}
