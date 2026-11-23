-- clone:    Berth keeps its own clone under BERTH_DATA_DIR/checkouts and may reset it.
-- existing: Berth runs from the folder already on the server (source_path); it only
--           fast-forwards that folder and never resets, cleans or switches branches there.
ALTER TABLE apps
    ADD COLUMN source_mode TEXT NOT NULL DEFAULT 'clone' CHECK (source_mode IN ('clone','existing'));
