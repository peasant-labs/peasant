CREATE TABLE sessions(session_id TEXT, model_harness TEXT, schema_version INTEGER);
INSERT INTO sessions VALUES('old-session','claude',1);
PRAGMA user_version = 9;
