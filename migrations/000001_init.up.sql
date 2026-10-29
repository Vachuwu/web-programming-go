CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE playlist_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title TEXT NOT NULL CHECK (btrim(title) <> ''),
    artist TEXT NOT NULL CHECK (btrim(artist) <> ''),
    url TEXT,
    added_by BIGINT NOT NULL REFERENCES users(id),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'playing', 'played', 'removed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    played_at TIMESTAMPTZ
);

CREATE TABLE votes (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    item_id BIGINT NOT NULL REFERENCES playlist_items(id),
    user_id BIGINT NOT NULL REFERENCES users(id),
    value SMALLINT NOT NULL CHECK (value IN (-1, 1)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (item_id, user_id)
);

CREATE TABLE playlist_state (
    id BIGINT PRIMARY KEY CHECK (id = 1),
    playback_version BIGINT NOT NULL CHECK (playback_version >= 1)
);

INSERT INTO playlist_state (id, playback_version) VALUES (1, 1);

CREATE UNIQUE INDEX one_playing_item
    ON playlist_items ((true)) WHERE status = 'playing';

CREATE INDEX playlist_items_queue_order_idx
    ON playlist_items (status, created_at, id);

CREATE INDEX playlist_items_history_idx
    ON playlist_items (status, played_at DESC, id DESC);
