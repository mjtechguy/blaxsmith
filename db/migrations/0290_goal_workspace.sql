-- Goals own pre-run conversations. Factory questions are data, never engine policy.
CREATE TABLE workflow_goals (
 organization_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 project_id uuid NOT NULL,
 request_key text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
 creation_sha256 text NOT NULL CHECK (creation_sha256 ~ '^[0-9a-f]{64}$'),
 title text NOT NULL CHECK (length(title) BETWEEN 1 AND 160),
 brief text NOT NULL CHECK (length(brief) BETWEEN 1 AND 16000),
 factory_id text NOT NULL,
 factory_version text NOT NULL,
 questions jsonb NOT NULL CHECK (jsonb_typeof(questions)='array' AND octet_length(questions::text)<=262144),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 created_by uuid NOT NULL REFERENCES identity_principals(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,id),
 UNIQUE (organization_id,project_id,created_by,request_key),
 FOREIGN KEY (organization_id,project_id) REFERENCES workflow_projects(organization_id,id)
);
CREATE INDEX workflow_goals_project ON workflow_goals(organization_id,project_id,created_at DESC,id DESC);
CREATE TABLE workflow_goal_entries (
 organization_id uuid NOT NULL,
 goal_id uuid NOT NULL,
 sequence bigint NOT NULL CHECK (sequence>1),
 request_key text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
 principal_id uuid NOT NULL REFERENCES identity_principals(id),
 kind text NOT NULL CHECK (kind IN ('message','answer','deferred')),
 question_id text NOT NULL DEFAULT '',
 option_ids jsonb NOT NULL CHECK (jsonb_typeof(option_ids)='array'),
 body text NOT NULL CHECK (length(body)<=4000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (organization_id,goal_id,sequence),
 UNIQUE (organization_id,goal_id,principal_id,request_key),
 FOREIGN KEY (organization_id,goal_id) REFERENCES workflow_goals(organization_id,id),
 CHECK ((kind='message' AND question_id='' AND option_ids='[]'::jsonb AND length(body)>0)
    OR (kind='answer' AND question_id<>'')
    OR (kind='deferred' AND question_id<>'' AND option_ids='[]'::jsonb AND body=''))
);
CREATE INDEX workflow_goal_answers ON workflow_goal_entries(organization_id,goal_id,question_id,sequence DESC) WHERE kind IN ('answer','deferred');
CREATE TRIGGER workflow_goal_entries_immutable BEFORE UPDATE OR DELETE ON workflow_goal_entries
 FOR EACH ROW EXECUTE FUNCTION workflow_events_append_only();
CREATE FUNCTION workflow_goal_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.organization_id,NEW.id,NEW.project_id,NEW.request_key,NEW.creation_sha256,NEW.title,NEW.brief,
     NEW.factory_id,NEW.factory_version,NEW.questions,NEW.created_by,NEW.created_at) IS DISTINCT FROM
    (OLD.organization_id,OLD.id,OLD.project_id,OLD.request_key,OLD.creation_sha256,OLD.title,OLD.brief,
     OLD.factory_id,OLD.factory_version,OLD.questions,OLD.created_by,OLD.created_at) THEN
   RAISE EXCEPTION 'goal identity and original brief are immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER workflow_goal_identity BEFORE UPDATE ON workflow_goals FOR EACH ROW EXECUTE FUNCTION workflow_goal_identity_immutable();
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['workflow_goals','workflow_goal_entries'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format($p$CREATE POLICY blaxsmith_tenant ON %I
   USING (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))
   WITH CHECK (current_setting('blaxsmith.system',true)='on' OR organization_id::text=current_setting('blaxsmith.org_id',true))$p$,t);
 END LOOP;
END $$;
