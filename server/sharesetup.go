package main

import (
	"net/http"
	"sort"
	"strings"
)

// Share setup: generate the commands an operator runs on their NFS server to
// create a dedicated export for ProxyCTL, restricted to exactly the cluster's
// node IPs. NFS volume mounts are performed by kubelet in the node's own
// network namespace, so the node InternalIPs — not pod IPs, which never leave
// the cluster overlay — are what the NFS server sees and what /etc/exports
// must list.
//
// The node set can grow after setup. The IPs the operator last exported to are
// snapshotted in the KeysStore; getKeysConfig compares that snapshot against
// the live node list and surfaces any node the share doesn't cover, with a
// regenerated exports line, instead of letting a pod on the new node hang at
// mount time with nothing naming the cause.

// parentDir names the export's parent for the warning comment ("/" and
// single-segment exports just show a generic example).
func parentDir(export string) string {
	i := strings.LastIndex(export, "/")
	if i <= 0 {
		return "/mnt/storage"
	}
	return export[:i]
}

// clusterNode is one node's name + the InternalIP its NFS traffic egresses from.
type clusterNode struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// listClusterNodes returns every node's InternalIP via the in-cluster SA
// (needs nodes get/list — see the ClusterRole). Sorted by IP for stable
// command output.
func listClusterNodes() ([]clusterNode, error) {
	out, err := runKubectlKeys("", "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var nodes []clusterNode
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		nodes = append(nodes, clusterNode{Name: fields[0], IP: fields[1]})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].IP < nodes[j].IP })
	return nodes, nil
}

// exportsLineFor renders the /etc/exports entry: the export followed by one
// host(options) clause per node IP, all on ONE line (exports entries are
// line-scoped). no_root_squash because kubelet mounts and creates subPath
// dirs as root — which is exactly why the entry lists specific node IPs
// rather than a subnet.
func exportsLineFor(export string, ips []string) string {
	var b strings.Builder
	b.WriteString(export)
	for _, ip := range ips {
		b.WriteString(" " + ip + "(rw,sync,no_subtree_check,no_root_squash)")
	}
	return b.String()
}

// renderShareCommands is the paste-ready block for the NFS server. When the
// node list couldn't be read, a <node-ip> placeholder keeps the shape of the
// commands honest instead of emitting an exports line that allows nothing.
func renderShareCommands(export string, ips []string) string {
	if len(ips) == 0 {
		ips = []string{"<node-ip>"}
	}
	var b strings.Builder
	b.WriteString("# Run on the NFS server, as root.\n")
	b.WriteString("# Creates ProxyCTL's dedicated share and exports it to ONLY the cluster nodes.\n")
	b.WriteString("#\n")
	b.WriteString("# IMPORTANT: this path must NOT sit inside a directory you already export.\n")
	b.WriteString("# NFS restrictions are per export path — clients of a broader parent export\n")
	b.WriteString("# (e.g. an all-hosts " + parentDir(export) + ") read straight through this one.\n")
	b.WriteString("# `exportfs -s` lists your exports; if any is a parent of this path, put\n")
	b.WriteString("# ProxyCTL's share somewhere outside it instead.\n")
	b.WriteString("mkdir -p " + export + "\n")
	b.WriteString("chmod 700 " + export + "\n")
	b.WriteString("cat >> /etc/exports <<'EOF'\n")
	b.WriteString(exportsLineFor(export, ips) + "\n")
	b.WriteString("EOF\n")
	b.WriteString("exportfs -ra\n")
	return b.String()
}

// nodesNotCovered returns the live nodes whose IP is missing from the
// snapshot of IPs the operator last exported to. A nil/empty snapshot means
// no exports line was ever generated — nothing to diff against.
func nodesNotCovered(live []clusterNode, covered []string) []clusterNode {
	if len(covered) == 0 {
		return nil
	}
	have := make(map[string]bool, len(covered))
	for _, ip := range covered {
		have[ip] = true
	}
	var missing []clusterNode
	for _, n := range live {
		if !have[n.IP] {
			missing = append(missing, n)
		}
	}
	return missing
}

// getShareSetup: GET /api/storage/share-setup?export=/mnt/ssd/ProxyCTL —
// the current node list plus the paste-ready share-creation commands for the
// given export (falling back to the saved share's export). Read-only; nothing
// is persisted here — the snapshot is taken when the share is SAVED.
func (a *API) getShareSetup(w http.ResponseWriter, r *http.Request) {
	export := strings.TrimSpace(r.URL.Query().Get("export"))
	if export == "" {
		_, export, _ = a.keys.Share()
	}
	if export != "" {
		if err := validateNFSExportPath(export); err != nil {
			a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		export = normalizeNFSExport(export)
	}

	out := map[string]any{"export": export}
	nodes, err := listClusterNodes()
	if err != nil {
		// Existing installs may run a ClusterRole from before nodes:get/list
		// was added — degrade to placeholder commands and say why.
		out["nodesError"] = "couldn't list cluster nodes (re-apply k8s/proxyctl.yaml to " +
			"grant the updated read-only RBAC): " + err.Error()
	}
	out["nodes"] = nodes
	if export != "" {
		ips := make([]string, len(nodes))
		for i, n := range nodes {
			ips[i] = n.IP
		}
		out["commands"] = renderShareCommands(export, ips)
		out["exportsLine"] = exportsLineFor(export, ips)
	}
	a.writeJSON(w, http.StatusOK, out)
}

// ackShareNodes: POST /api/storage/share-setup/ack — the operator says the
// share's /etc/exports line now covers the CURRENT node set (they just pasted
// the updated line). Re-snapshots the live node IPs, which clears the
// uncovered-nodes warning.
func (a *API) ackShareNodes(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.keys.Share(); !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no NFS share is configured"})
		return
	}
	nodes, err := listClusterNodes()
	if err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "couldn't list cluster nodes: " + err.Error()})
		return
	}
	ips := make([]string, len(nodes))
	for i, n := range nodes {
		ips[i] = n.IP
	}
	if err := a.keys.SetNodeIPs(ips); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "nodeIPs": ips})
}
