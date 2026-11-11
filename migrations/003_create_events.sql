CREATE TABLE events (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_type TEXT        NOT NULL,
    payload    JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deliveries (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id     BIGINT      NOT NULL REFERENCES events(id),
    endpoint_id  BIGINT      NOT NULL REFERENCES endpoints(id),
    status       TEXT        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'processing', 'succeeded', 'dead')),
    attempts     INTEGER     NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by    TEXT,
    locked_until TIMESTAMPTZ,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);

-- Worker ищет ожидающие доставки по (status, available_at).
CREATE INDEX idx_deliveries_status_available
    ON deliveries (status, available_at);

-- Выборки доставок конкретного endpoint.
CREATE INDEX idx_deliveries_endpoint_id
    ON deliveries (endpoint_id);
