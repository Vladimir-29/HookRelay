-- Зашифрованный AES-256-GCM секрет endpoint для HMAC-подписи вебхуков.
-- Формат значения: nonce (12 байт) || ciphertext+tag, колонка BYTEA.
-- NULL допустим: endpoints, созданные до этой миграции, не имеют секрета;
-- их можно перевыпустить через POST /api/v1/endpoints/{id}/rotate-secret.
ALTER TABLE endpoints
    ADD COLUMN secret_encrypted BYTEA;
