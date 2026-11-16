-- История попыток доставки: по одной записи на каждый HTTP-вызов.
CREATE TABLE delivery_attempts (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    delivery_id   BIGINT      NOT NULL REFERENCES deliveries(id),
    attempt       INTEGER     NOT NULL,
    status_code   INTEGER,
    response_body TEXT,
    duration_ms   INTEGER     NOT NULL,
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_delivery_attempts_delivery_id
    ON delivery_attempts (delivery_id);
