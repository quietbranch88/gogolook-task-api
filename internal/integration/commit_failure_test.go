package integration_test

import (
	"context"
	"testing"
)

func TestIdempotentCommitFailureRollsBackTaskAndKey(t *testing.T) {
	f := setup(t)
	// A deferred trigger fails during COMMIT, after both task and key SQL have run.
	// This is a real database rejection, not a mocked Commit return value.
	_, err := f.pool.Exec(context.Background(), `
CREATE FUNCTION fail_task_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'synthetic commit rejection' USING ERRCODE='40001'; END $$;
CREATE CONSTRAINT TRIGGER reject_task_commit AFTER INSERT ON tasks
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_task_commit();`)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"name":"retry after aborted commit","status":0}`
	status, _, err := keyedPost(f.server.URL, f.a, "101", "commit-failure", body)
	if err != nil || status != 500 {
		t.Fatalf("failed commit must not report success: status=%d err=%v", status, err)
	}
	var tasks, keys int
	if err = f.pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM tasks WHERE tenant_id=101),(SELECT count(*) FROM task_create_keys WHERE tenant_id=101)").Scan(&tasks, &keys); err != nil || tasks != 0 || keys != 0 {
		t.Fatalf("aborted commit left tasks=%d keys=%d err=%v", tasks, keys, err)
	}
	if _, err = f.pool.Exec(context.Background(), "DROP TRIGGER reject_task_commit ON tasks"); err != nil {
		t.Fatal(err)
	}
	status, created, err := keyedPost(f.server.URL, f.a, "101", "commit-failure", body)
	if err != nil || status != 201 || created.ID < 1 {
		t.Fatalf("retry did not create after rollback: status=%d err=%v", status, err)
	}
	if err = f.pool.QueryRow(context.Background(), "SELECT count(*) FROM task_create_keys k JOIN tasks t ON t.id=k.task_id AND t.tenant_id=k.tenant_id WHERE k.tenant_id=101 AND t.name=$1", created.Name).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("retry durable task/key count=%d err=%v", keys, err)
	}
}
