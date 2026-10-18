-- Learned routing (ORCHESTRATOR.md "Learned routing", FR-O4).
-- Each run keeps the arm it pulled (task type, difficulty bucket, driver model) and the reward it
-- added to the bandit, so a changed outcome (a guess, then the user's rating) moves the counts
-- instead of adding to them twice.
ALTER TABLE runs ADD COLUMN task_type TEXT;
ALTER TABLE runs ADD COLUMN difficulty_bucket INTEGER;
ALTER TABLE runs ADD COLUMN reward REAL;                -- NULL = not counted in the bandit
