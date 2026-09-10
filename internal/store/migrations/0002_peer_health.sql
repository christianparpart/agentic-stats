-- Convergence health, so "is this mesh actually working" is answerable.
--
-- last_seen already recorded when a peer was last heard from, which includes a
-- beacon announcement. That is not the same question: a peer can be announcing
-- itself on the network every thirty seconds while every attempt to converge
-- with it fails, and the node would report it as seen and look healthy. These
-- columns record whether data actually moved.
--
-- last_error is kept alongside rather than instead of the success, because the
-- useful report is "converged an hour ago, and has been refusing since" -- one
-- without the other cannot say that.
ALTER TABLE peers ADD COLUMN last_converged TEXT;
ALTER TABLE peers ADD COLUMN last_error TEXT;

-- What the peer said it held at that exchange, as a version vector.
--
-- Stored so replication lag can be computed rather than guessed: without the
-- peer's own view there is no way to tell "we are in step" from "we have not
-- heard the half of it". Opaque JSON here on purpose -- the shape belongs to
-- the sync layer, and the store's job is to keep it, not to interpret it.
ALTER TABLE peers ADD COLUMN last_vector TEXT;
