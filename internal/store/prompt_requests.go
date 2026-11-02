package store

import "fmt"

// ClaimPromptRequest durably reserves an ID before any side effect. A crash
// between reservation and completion leaves an uncertain receipt, never a resend.
func (s *Store) ClaimPromptRequest(sessionID, requestID, fingerprint string) (bool, string, error) {
	result, err := s.db.Exec(`INSERT OR IGNORE INTO prompt_requests(sessionId, requestId, fingerprint) VALUES(?,?,?)`, sessionID, requestID, fingerprint)
	if err != nil {
		return false, "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if n == 1 {
		return true, "", nil
	}
	var stored string
	var receipt *string
	if err := s.db.QueryRow(`SELECT fingerprint, result FROM prompt_requests WHERE sessionId=? AND requestId=?`, sessionID, requestID).Scan(&stored, &receipt); err != nil {
		return false, "", err
	}
	if stored != fingerprint {
		return false, "", fmt.Errorf("request ID reused with different prompt content")
	}
	if receipt == nil {
		return false, "", fmt.Errorf("prompt delivery is still unconfirmed; check the chat and queue before retrying")
	}
	return false, *receipt, nil
}

func (s *Store) CompletePromptRequest(sessionID, requestID, result string) error {
	_, err := s.db.Exec(`UPDATE prompt_requests SET result=? WHERE sessionId=? AND requestId=?`, result, sessionID, requestID)
	return err
}
