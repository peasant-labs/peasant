package store

// Retirement keeps annotation values and targets available to historical readers.
const migrationV58 = `ALTER TABLE annotations ADD COLUMN retired_at INTEGER;`
