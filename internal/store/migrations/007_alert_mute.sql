-- Pause alert emails for one service. Checks, graphs and the alerts on the Services page continue.
-- NULL: not paused. A time in the past: the pause has ended. Year 9999: until resumed by hand.
ALTER TABLE services ADD COLUMN alerts_muted_until TIMESTAMPTZ;
