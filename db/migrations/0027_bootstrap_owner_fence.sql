CREATE FUNCTION bootstrap_owner_fenced_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.cluster_id, NEW.attempt_id) IS DISTINCT FROM (OLD.cluster_id, OLD.attempt_id) OR
       NEW.owner_generation <> OLD.owner_generation + 1 THEN
        RAISE EXCEPTION 'bootstrap owner changes must preserve identity and advance generation';
    END IF;
    IF NEW.active THEN
        RETURN NEW;
    END IF;
    IF OLD.active AND (NEW.actor_atespace, NEW.actor_name, NEW.actor_uid) IS NOT DISTINCT FROM
       (OLD.actor_atespace, OLD.actor_name, OLD.actor_uid) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'bootstrap owner deactivation cannot change actor identity';
END $$;

CREATE TRIGGER bootstrap_owner_fenced_update
    BEFORE UPDATE ON bootstrap_owners
    FOR EACH ROW EXECUTE FUNCTION bootstrap_owner_fenced_update();
