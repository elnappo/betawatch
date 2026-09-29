-- Requires PRAGMA foreign_keys = ON on every connection.

-- Only id, uid and user are filled from the minute diffs. The other
-- columns, including timestamp, are NULL until a metadata source is added.
CREATE TABLE IF NOT EXISTS changesets (
    id BIGINT PRIMARY KEY,
    timestamp DATETIME,
    uid BIGINT,
    user TEXT,
    comment TEXT,
    created_by TEXT,
    imagery_used TEXT,
    source TEXT,
    bot BOOLEAN,
    locale TEXT,
    review_requested BOOLEAN,
    data_used TEXT,
    hashtags TEXT,
    host TEXT,
    changesets_count INTEGER,
    changes_count INTEGER,
    created_count INTEGER,
    modified_count INTEGER,
    deleted_count INTEGER
);

-- Live elements, latest version only. A deleted element is removed here.
CREATE TABLE IF NOT EXISTS elements (
    id BIGINT NOT NULL,
    type CHAR(1) NOT NULL CHECK (type IN ('n', 'w', 'r')),
    version INTEGER NOT NULL,
    lat REAL,
    lon REAL,
    uid BIGINT,
    user TEXT,
    timestamp DATETIME NOT NULL,
    changeset_id BIGINT NOT NULL REFERENCES changesets(id),
    tags TEXT, -- JSON
    PRIMARY KEY (id, type)
);

-- Superseded versions. A delete moves the row here from elements with
-- deleted = 1.
CREATE TABLE IF NOT EXISTS elements_history (
    id BIGINT NOT NULL,
    type CHAR(1) NOT NULL CHECK (type IN ('n', 'w', 'r')),
    version INTEGER NOT NULL,
    lat REAL,
    lon REAL,
    uid BIGINT,
    user TEXT,
    timestamp DATETIME NOT NULL,
    changeset_id BIGINT NOT NULL REFERENCES changesets(id),
    tags TEXT, -- JSON
    deleted BOOLEAN NOT NULL DEFAULT 0,
    PRIMARY KEY (id, type, version)
);

-- The primary keys already index (id, type) and (id, type, version).
CREATE INDEX IF NOT EXISTS idx_elements_timestamp ON elements(timestamp);
