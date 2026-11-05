package restv1

// Cluster is the status of the raft cluster as the node that answers sees it
// (GET /api/v1/cluster). The leader's address is its raft address, as the
// configuration writes it; both leader fields are empty while the node knows no
// leader. ConfigFingerprint is the fingerprint of the settings that every
// replica must share, as the answering node holds them: the hex SHA-256 over
// the digests of their groups. Nodes whose fingerprints differ do not share
// them.
type Cluster struct {
	NodeID            string          `json:"node_id"`
	Role              string          `json:"role"`
	LeaderID          string          `json:"leader_id,omitempty"`
	LeaderAddress     string          `json:"leader_address,omitempty"`
	Members           []ClusterMember `json:"members"`
	LastIndex         uint64          `json:"last_index"`
	ConfigFingerprint string          `json:"config_fingerprint"`
}

// ClusterMember is a server of the cluster's raft configuration: its ID, its
// raft address as written, and whether it votes (voter, nonvoter or staging).
type ClusterMember struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Suffrage string `json:"suffrage"`
}

// AddClusterMemberRequest is the body of POST /api/v1/cluster/members: the ID
// and the raft address (host:port) of the voter to add.
type AddClusterMemberRequest struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}
