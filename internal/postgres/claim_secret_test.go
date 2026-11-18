package postgres

import (
	"bytes"
	"context"
	"testing"
	"time"

	"hookrelay/internal/secrets"
)

// TestClaimDeliveriesSecret: claim возвращает зашифрованный секрет
// endpoint, и он расшифровывается Box'ом в исходный секрет.
func TestClaimDeliveriesSecret(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	box, err := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	raw := bytes.Repeat([]byte{11}, 32)
	ct, err := box.Seal(raw)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	id := deliveryFixture(t, store, "pending", nil, nil)
	if _, err := store.pool.Exec(ctx, `
		UPDATE endpoints SET secret_encrypted = $2
		WHERE id = (SELECT endpoint_id FROM deliveries WHERE id = $1)`,
		id, ct); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	claimed, err := store.ClaimDeliveries(ctx, "w-secret", 100, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	d, ok := findClaimed(claimed, id)
	if !ok {
		t.Fatal("fixture delivery not claimed")
	}
	if len(d.SecretEncrypted) == 0 {
		t.Fatal("SecretEncrypted is empty")
	}
	got, err := box.Open(d.SecretEncrypted)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("decrypted secret mismatch")
	}
}
