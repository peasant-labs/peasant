CREATE TABLE sessions(session_id TEXT, model_harness TEXT, schema_version INTEGER);
INSERT INTO sessions VALUES('old-session','claude',1);
INSERT INTO sessions VALUES('current-metadata-session','claude',9);
PRAGMA user_version = 9;
