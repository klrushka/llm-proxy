package loadbench

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Record is one synthetic payload_id/original/mask record. The mask is filled
// in during prewarm by masking the original through the service.
type Record struct {
	ID       string
	Original string
	Mask     string
}

// Pool is a bounded set of synthetic records. It is prepared (prewarmed) before
// timing so the timed run only issues requests against already-known records,
// bounding server-side record growth to PoolSize.
type Pool struct {
	records []Record
	next    atomic.Uint64
}

// NewPool builds a pool of size synthetic records with distinct payload_ids and
// synthetic PII originals. No real PII is ever generated.
func NewPool(size int) *Pool {
	records := make([]Record, size)
	for i := 0; i < size; i++ {
		records[i] = Record{
			ID:       fmt.Sprintf("load-%d", i),
			Original: syntheticPayload(i),
		}
	}
	return &Pool{records: records}
}

// Prewarm masks every original through the service and stores the returned mask
// on the record. It must complete before timing begins. A failure on any record
// fails the whole run.
func (p *Pool) Prewarm(ctx context.Context, client *Client) error {
	for i := range p.records {
		req := Request{Payload: p.records[i].Original, PayloadID: p.records[i].ID}
		result, err := client.Do(ctx, req)
		if err != nil {
			return fmt.Errorf("prewarm record %d (%s): %w", i, p.records[i].ID, err)
		}
		p.records[i].Mask = result
	}
	return nil
}

// Next returns the next deterministic request and the expected result. It
// alternates between the mask path (send the original, expect the stored mask)
// and the restore path (send the mask, expect the original), cycling through
// the bounded pool. Each pair of calls uses the same record: original(record 0),
// mask(record 0), original(record 1), mask(record 1), and so on. It is safe for
// concurrent use.
func (p *Pool) Next() (Request, string) {
	i := p.next.Add(1) - 1
	rec := p.records[(i/2)%uint64(len(p.records))]
	if i%2 == 0 {
		return Request{Payload: rec.Original, PayloadID: rec.ID}, rec.Mask
	}
	return Request{Payload: rec.Mask, PayloadID: rec.ID}, rec.Original
}

// syntheticPayload builds a distinct synthetic Russian PII payload containing an
// email and a phone that the rules pipeline detects and masks. The index makes
// each record unique so the pool holds distinct records.
func syntheticPayload(i int) string {
	return fmt.Sprintf(
		"Клиент Тестов Тест Тестович, телефон +7 900 123-45-%02d, email test%d@example.com",
		i%100, i,
	)
}
