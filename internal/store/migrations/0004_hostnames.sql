-- Names for the addresses peers answer on.
--
-- Keyed by address rather than by peer, because an address is what a PTR
-- record describes and because the two change independently: a laptop that
-- moves between networks arrives on a different address, while the machine
-- that used to hold the old one is still correctly named by the old row.
--
-- The point of writing them down is that nothing has to ask. A reverse lookup
-- is a round trip to a resolver that may be gone -- on a machine that just
-- left a VPN, reliably so, which is exactly when someone runs `status` to find
-- out what is wrong. Resolving there would make a report about the archive
-- wait on DNS and time out with it.
CREATE TABLE hostnames (
    ip          TEXT PRIMARY KEY,
    -- Empty means asked, and the address has no name. That is an answer worth
    -- keeping: most addresses have none, and re-asking every sweep puts a
    -- steady trickle of doomed queries on a resolver that may not be there.
    hostname    TEXT NOT NULL,
    resolved_at TEXT NOT NULL
) WITHOUT ROWID;
