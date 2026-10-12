-- Initial schema for this application. schema_migrations is created by the Go runner.
CREATE TABLE tenants (
 id bigint PRIMARY KEY,
 name text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT statement_timestamp()
);
CREATE TABLE users (
 id bigint PRIMARY KEY,
 created_at timestamptz NOT NULL DEFAULT statement_timestamp()
);
CREATE TABLE memberships (
 tenant_id bigint NOT NULL REFERENCES tenants(id),
 user_id bigint NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
 PRIMARY KEY (tenant_id,user_id)
);
CREATE TABLE api_keys (
 token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash)=32),
 user_id bigint NOT NULL REFERENCES users(id),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT statement_timestamp()
);
CREATE TABLE tasks (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 tenant_id bigint NOT NULL REFERENCES tenants(id),
 name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 1000 AND name ~ '[^[:space:]]'),
 status smallint NOT NULL CHECK (status IN (0,1)), -- 0 = not done, 1 = done
 detail text NOT NULL DEFAULT '' CHECK (char_length(detail) <= 10000),
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT statement_timestamp()
);
CREATE INDEX tasks_tenant_id_id ON tasks(tenant_id,id);
-- Retain only operation digests and the task reference, not copied task data.
CREATE TABLE task_create_keys (
 tenant_id bigint NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 key_hash bytea NOT NULL CHECK (octet_length(key_hash)=32),
 input_hash bytea NOT NULL CHECK (octet_length(input_hash)=32),
 task_id bigint REFERENCES tasks(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
 PRIMARY KEY (tenant_id,user_id,key_hash)
);
CREATE INDEX task_create_keys_task_id ON task_create_keys(task_id);

-- Restore database-owned timestamps before comparing actual row contents.
-- No-op updates preserve time; creation time can never be changed or cleared.
CREATE FUNCTION preserve_creation_and_stamp_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 NEW.created_at := OLD.created_at;
 NEW.updated_at := OLD.updated_at;
 IF NEW IS DISTINCT FROM OLD THEN
  NEW.updated_at := statement_timestamp();
 END IF;
 RETURN NEW;
END;
$$;

CREATE TRIGGER tenants_timestamps BEFORE UPDATE ON tenants
 FOR EACH ROW EXECUTE FUNCTION preserve_creation_and_stamp_update();
CREATE TRIGGER api_keys_timestamps BEFORE UPDATE ON api_keys
 FOR EACH ROW EXECUTE FUNCTION preserve_creation_and_stamp_update();
CREATE TRIGGER tasks_timestamps BEFORE UPDATE ON tasks
 FOR EACH ROW EXECUTE FUNCTION preserve_creation_and_stamp_update();
