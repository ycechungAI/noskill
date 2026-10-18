-- Which learned skills each eval task used (block 2c): the regression gate rolls back only skills
-- that took part in a task that regressed, and the learning exit check reports the learned share.
ALTER TABLE eval_results ADD COLUMN learned TEXT; -- comma-separated learned skill names, "" = none
