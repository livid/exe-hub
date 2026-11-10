package store

import (
	"testing"
)

// TestReader: the second pool reads what the store's connection wrote,
// while that connection is in a transaction of its own, and writes
// nothing; a store in memory has none.
func TestReader(t *testing.T) {
	s := openTest(t)
	rd, err := s.Reader(3)
	if err != nil || rd == nil {
		t.Fatalf("Reader: %v, %v", rd, err)
	}
	defer rd.Close()
	if n := rd.Stats().MaxOpenConnections; n != 3 {
		t.Fatalf("%d connections, want 3", n)
	}
	if _, err := s.db.Exec(`CREATE TABLE reader_t (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO reader_t VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin() // the store's one connection is taken
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO reader_t VALUES (8)`); err != nil {
		t.Fatal(err)
	}
	var n, sum int
	if err := rd.QueryRow(`SELECT COUNT(*), SUM(n) FROM reader_t`).Scan(&n, &sum); err != nil || n != 1 || sum != 7 {
		t.Fatalf("read beside a write: %d rows summing %d (%v), want the one committed", n, sum, err)
	}
	if _, err := rd.Exec(`INSERT INTO reader_t VALUES (9)`); err == nil {
		t.Fatal("the reader wrote")
	}
	if one, err := s.Reader(0); err != nil || one == nil {
		t.Fatalf("Reader(0): %v", err)
	} else {
		if n := one.Stats().MaxOpenConnections; n != 1 {
			t.Fatalf("Reader(0) has %d connections, want 1", n)
		}
		one.Close()
	}

	mem, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	if rd, err := mem.Reader(2); rd != nil || err != nil {
		t.Fatalf("a store in memory gave a reader (%v)", err)
	}
}
