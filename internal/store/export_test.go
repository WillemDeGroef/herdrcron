package store

import "fmt"

// SetUserVersionForTest overwrites PRAGMA user_version, to simulate a
// database written by a newer herdrcron.
func (s *Store) SetUserVersionForTest(v int) error {
	_, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}
