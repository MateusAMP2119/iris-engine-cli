package store

// LaneMember is one persisted composer row: a pipeline's lane membership, as
// the dead-letter blast walk (and any other lane-shaped read) consumes it.
type LaneMember struct {
	// Lane is the lane's name.
	Lane string
	// Pipeline is the member pipeline's name.
	Pipeline string
}

// laneMembersSQL reads the persisted composer rows, in lane then walk (pos)
// order.
const laneMembersSQL = `SELECT lane, pipeline FROM lanes ORDER BY lane, pos`
