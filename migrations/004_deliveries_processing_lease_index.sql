-- Частичный индекс для поиска processing-доставок с истёкшим lease:
-- Worker проверяет их при каждом ClaimDeliveries.
CREATE INDEX idx_deliveries_processing_lease
    ON deliveries (locked_until)
    WHERE status = 'processing';
