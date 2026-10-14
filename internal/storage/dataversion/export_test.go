package dataversion

import "context"

// ExecOnProbeConnForTest runs statement on the probe's own connection, so a
// test can prove that connection is read-only.
func ExecOnProbeConnForTest(p *Probe, statement string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.conn.ExecContext(context.Background(), statement)
	return err
}
