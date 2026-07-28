# Changelog

All notable changes to ProxyCTL are documented here. This file is the
human-readable companion to `server/releases.json`, which is embedded into
the binary and surfaced in-app by the update tool ("What's new"). **Keep
the two in sync** — when you add a release here, add the matching
structured entry to `releases.json` (same `version`).

The newest release is first. `version` matches the build stamp injected
via `-X main.version=$(VERSION)` (a git tag like `v0.6.16`, or a short
commit SHA for untagged builds).

Only real releases are listed — there is no "Unreleased" section. A build
whose version doesn't match a tagged entry (any SHA/dev build between
tags) resolves to the **newest release** in the in-app notes, so What's
new always describes something that actually shipped. Write changes into
the release entry as you cut it, not into a staging section beforehand.

## [v0.6.17] - 2026-07-28

> A documentation accuracy pass. The README and SECURITY model still described the old credential story — ambient ssh-agent, Traefik IngressRoutes, cluster-wide write — none of which is how ProxyCTL works any more. They now match the app: ProxyCTL generates its own SSH keypair whose private half never leaves it, publishes web routes as Cloudflare Tunnel ingress rules, and holds no cluster-wide write access at all. No functional change.

### Fixed

- **Stale code comments pointing at Traefik** — The web-route store was still commented as rendering Traefik IngressRoutes. Comments only, but the kind that sends the next reader looking for code that no longer exists.

### Changed

- **README describes the credentials ProxyCTL actually uses** — It claimed ProxyCTL borrows your ambient ssh + kubectl and holds zero standing credentials. In reality it generates its own ed25519 keypair for the droplet — the private half never leaves the app and no API returns it — and reaches your cluster through its own ServiceAccount. The droplet-creation steps said your SSH key was required because ProxyCTL uses your ssh-agent; it is now correctly described as how you get a root shell, with ProxyCTL installing its own public half via the Setup wizard. The installer environment variables are documented in full.
- **SECURITY model reflects namespaced RBAC** — The threat model still described web routes dropping a Traefik IngressRoute next to each backend app, which needed cluster-wide write for that resource type. Web routes are published as Cloudflare Tunnel ingress rules now, so that permission is gone: ProxyCTL has no cluster-wide write to anything, and every workload it creates lives in its own namespace. Cluster-wide read is spelled out for what it is — the target picker browsing Services and Endpoints, and reading node InternalIPs to generate a restricted /etc/exports line.
## [v0.6.16] - 2026-07-27

> Entries are editable in place instead of delete-and-re-add, Apply only touches the half of ProxyCTL you are looking at, and it no longer re-pushes every gateway to add one. Plus a web app status light that reflects real reachability rather than always showing a warning.

### Fixed

- **Editing an entry no longer re-keys its tunnel** — The update endpoint replaced the whole stored entry with the request body, so a client that sent only the operator-facing fields blanked the applier-managed ones. That reallocated the entry's pinned tunnel address and forced its gateway to regenerate its key — the tunnel dropped and the droplet peer had to be re-registered, for an edit that should have changed a label. The server now carries those fields forward.
- **Web app status light reflects real reachability** — Every enabled web app rendered with the amber "down" indicator whether or not the site was actually reachable — the row had no reachability data to colour it with, even though ProxyCTL was already probing each hostname every minute. The list now carries that state, so the light reads online, unreachable, or (for a route with no sample yet) simply enabled.
- **Unapplied-changes bar appears after a web app edit** — The bar is driven by a payload that only refreshed after proxy-entry changes, so editing a web app left it hidden with a real pending change behind it — it appeared only once something else happened to refresh, usually the Apply click itself. Web app add, edit, enable/disable, and delete now refresh it immediately.
- **NFS paths with a trailing or doubled slash** — The share and keys-folder fields were concatenated without normalizing, so an export typed as "/mnt/ssd/" or a folder typed as "/ProxyCTL/Keys" produced a doubled slash in the path shown and saved. Both are now normalized as you leave the field and again before saving, matching what the server stores.

### Added

- **Edit proxy entries and web apps in place** — Changing a port, target, or hostname meant deleting the entry and adding it back, which tore down its gateway and re-keyed its tunnel for what was often a one-character correction. Both lists now have an Edit button that loads the entry into the form; saving keeps its identity.

### Changed

- **Apply is scoped to the page you are on** — Proxy Entries (WireGuard gateways and droplet rules) and Web Apps (a Cloudflare Tunnel) are independent, but a single global Apply meant publishing a web app also re-pushed every proxy entry — minutes of SSH for a change that could not affect them. Each page now applies its own half, and the unapplied-changes indicator is tracked per half so one does not report the other as up to date.
- **Apply reconciles what changed, with Full reconcile as the escape hatch** — Apply walked every entry through its gateway phases even when only one had changed. On unchanged entries each step was a no-op, so nothing was disrupted, but adding one entry to a dozen meant watching the whole set scroll past. Apply now reconciles entries that are new, edited, or missing a gateway key, and names the ones it skipped; the droplet's WireGuard config and firewall rules are still rebuilt in full every time, since they are one file and one chain set covering all peers. Full reconcile forces every gateway through — for a gateway deleted outside ProxyCTL, an apply that failed partway, or one tunnel dead while the rest work.
- **The admin page remembers your tab** — A refresh always returned you to Configuration. It now reopens the tab you were on, and a first visit opens Proxy Entries — the thing you came for — rather than setup you touch once.
- **Uptime bars respond to hover, with readable timestamps** — Hovering a bucket now raises it and opens a tooltip with that bucket's time range and result, replacing a native tooltip that took about a second to appear and could not distinguish an up bucket from a down one by colour. The time labels under each graph were also the dimmest tone on the palette; they now match the rest of the interface.
- **Password minimum is 8 characters, matching GameCTL** — ProxyCTL required 12 characters while GameCTL required 8, so the two disagreed about what a valid password was. Both now require 8. This is a reduction for ProxyCTL — existing passwords are unaffected, and the floor applies only when one is set or changed.

## [v0.6.13] - 2026-07-22

> Re-installing onto a share/directory that already has ProxyCTL data now tells you what's there before you commit, plus SSH/UFW documentation updates.

### Added

- **Storage step warns before reusing an existing directory's data** — Re-installing onto the same NFS share as a prior install (or one you're re-pointing at) reuses whatever's already there — that was always safe, since moving in never deletes or overwrites anything, but it happened silently. Testing a share now also reports how much is already in it (app config files, gateway WireGuard key folders), shown as an info panel, and 'Save & move in' asks you to confirm before proceeding when it finds something. A genuinely empty share skips this entirely.

### Changed

- **README/SECURITY docs updated for the SSH access-control changes** — SECURITY.md now documents the droplet SSH access model directly: tunnel-only vs. the public-IP allow-list, why fail2ban only has anything to see under the latter, and the UFW leftover-rule detector's verify-and-revert safety net. README's outdated 'UFW automation is gone' line is corrected.

## [v0.6.12] - 2026-07-22

> New ways to actually reach and manage the droplet: a ready-to-run SSH command on every personal-access device, a detector that finds and safely removes leftover manual SSH lockdowns, and fail2ban copy that's honest about when it's actually protecting anything.

### Added

- **Copy-paste SSH connect command on personal-access devices** — The 'Activate it from the command line' block for a personal-access device now ends with a real, ready-to-run `ssh -i ~/.ssh/proxyctl-<device> root@10.8.0.1` line instead of leaving the tunnel IP and key path to prose — 10.8.0.1 is the droplet's address on the tunnel, not its public IP, and that distinction was the single most common point of confusion. Also warns that a .conf downloaded over plain HTTP (no cert configured yet) may get silently withheld by the browser until you click Keep/Allow.
- **Detects and safely removes leftover UFW rules that predate ProxyCTL** — Some droplets were locked down by hand with a UFW rule restricting port 22 to a specific IP before ProxyCTL managed SSH access at all. That rule is invisible to and unmanaged by either lockdown option (tunnel-only or the public-IP allow-list) and breaks the operator's own access the moment that IP changes. The 'Restrict SSH access' setup step now detects a rule like this and offers to remove it — never a wide-open rule, never anything for another port, never UFW itself. Before touching anything it snapshots the real rule files and, after the change, verifies a fresh SSH connection still works, automatically reverting to the exact prior rules if it doesn't — the same safety pattern already used when applying either lockdown option.
- **Proxy Entries totals bar** — A 'Totals:' line under the Proxy Entries subtitle now shows summed connections and total ↓in/↑out traffic across every entry, refreshing on the same 5-second cadence as the per-entry numbers below it.

### Changed

- **SSH tab relabels to 'WireGuard Access' under tunnel-only lockdown** — fail2ban only ever sees traffic when SSH is reachable by IP — under tunnel-only lockdown, the firewall drops unauthorized connections before they ever reach sshd, so fail2ban has nothing to log. The SSH Security tab now hides the fail2ban card entirely in that mode and relabels itself 'WireGuard Access', since at that point the tab is really just device management. Choosing the public-IP allow-list instead now also (re)applies fail2ban's ban policy automatically, so it's guaranteed to be live rather than depending on a separate step — without restarting it on every edit, which briefly re-opens the firewall to already-banned IPs.
- **Web Apps table links straight to each hostname** — Each row now shows its full https:// URL as a clickable link instead of a bare hostname.

## [v0.6.8] - 2026-07-19

> Entries are now called Proxy Entries (they tunnel any TCP/UDP service, not just games), plus a 'Need a droplet?' guide in setup and an MIT license.

### Fixed

- **CI build failure from a missing embedded-UI placeholder** — The go job failed with 'pattern all:web/dist: no matching files found'. main.go embeds web/dist and CI builds Go without running the UI build first, so the directory must exist in a fresh checkout — but its tracked .gitkeep had been deleted, because Vite's emptyOutDir wipes the directory on every UI build. The placeholder is restored and a Vite plugin now re-creates it after each build so it cannot go missing again.

### Added

- **'Need a droplet?' guide on the setup Connect step** — A droplet is required but the wizard never said what to buy. The Connect step now has a collapsible section with a direct link to DigitalOcean's droplet creator, the recommended specs (Ubuntu 24.04 LTS, Basic/Regular, $4/mo — 1 vCPU, 512 MB RAM, 10 GB SSD, 500 GB transfer), a note that bandwidth is the one spec worth sizing up, and the plan screenshot. Collapsed by default so operators who already have a droplet are not slowed down.
- **MIT license** — ProxyCTL now ships a LICENSE file (MIT), so the project is explicitly free to use, fork and self-host.

### Changed

- **L4 entries renamed from 'Games' to 'Proxy Entries'** — Every label called these game entries even though they proxy any TCP/UDP service: the tab, the add-form step ('1 · Game'), the template picker, and the DNS/rebind/ports hints. The tab is now 'Proxy Entries', the step is '1 · Entry', and the wording talks about entries and services throughout. The game presets stay — they are just port presets now, not the identity of the feature.

## [v0.6.7] - 2026-07-18

> Cleaner What's-new: released builds now show exactly what shipped, and the notes for the last two releases are filled in.

### Fixed

- **Release notes backfilled for v0.6.5 and v0.6.6** — Both releases shipped without a structured notes entry, so builds running them fell back to the Unreleased placeholder in the What's-new panel. Their entries now exist (below), and the running version highlights correctly.

### Changed

- **Released builds hide the 'Unreleased' notes section** — The in-app What's-new list only shows the Unreleased staging entry on dev builds. On a tagged release it was either an empty 'changes will appear here' placeholder or — when a release shipped without a notes entry — got mislabeled as 'This build'. Tagged builds now list tagged releases only.

## [v0.6.6] - 2026-07-18

> The sticky header no longer falls apart around the What's new / update controls.

### Fixed

- **Header layout no longer malforms** — The header bar is a no-wrap flex row; once the What's new + Check for updates controls (and their transient status message) landed, its content outgrew the shell and the tagline/buttons word-wrapped, stacking the sticky header into a multi-line mess. Now the tagline is the only element that gives up space (ellipsized, hidden on narrow windows), every control is shrink-proof, and the status flash is capped with an ellipsis.
- **No more empty apply-mode pill** — The apply-mode badge rendered as a hollow bordered oval before its data loaded; it now only renders once the mode is known.

## [v0.6.5] - 2026-07-18

> SPT + Fika (Tarkov co-op) joins the game presets, and the README got a full walkthrough facelift.

### Added

- **SPT + Fika game preset** — Single Player Tarkov with the Fika co-op mod is now a first-class preset: pick it when creating an entry and the right ports and protocol come pre-filled.

### Changed

- **README walkthrough facelift** — Real screenshots (tunnels dashboard, setup wizard, droplet plan), the exact $4 DigitalOcean droplet spec, a step-by-step scoped Cloudflare API token guide, and clearer GameCTL pairing guidance.

## [v0.6.4] - 2026-07-17

> You can now see — and pick — the public interface ProxyCTL binds its droplet firewall rules to.

### Added

- **Public interface picker on the Prepare step** — After preparing the droplet, the wizard shows which network interface was auto-detected as the public one. Boxes with a single interface just display it; boxes with several offer a dropdown listing each interface with its address, so you can pin the right one explicitly. A pinned choice wins over auto-detection — Apply then only warns if detection disagrees — and clearing the pin returns to auto. Either way it takes effect on the next Apply.

## [v0.6.3] - 2026-07-17

> The droplet's public network interface is now detected automatically, and two entries can no longer silently fight over the same public port.

### Fixed

- **Public interface auto-detection on the droplet** — The firewall/NAT rules bind to the droplet's public network interface, which was assumed to be eth0 — true on DigitalOcean, wrong on providers that name it ens3/enX0-style, where public traffic then silently never entered ProxyCTL's rules. The droplet's actual egress interface is now detected during Prepare and re-checked on every Apply, so re-renders always use the real one.
- **Cross-entry port conflicts are rejected at save time** — Two enabled entries could claim the same public port; on the droplet only one rule can win, and the loser failed silently. Saving or enabling an entry whose port/protocol collides with another enabled entry now fails with a message naming the conflicting entry. Sharing one hostname across entries with different ports — for example LiveKit signaling and media split into two entries — remains fully supported: DNS points at the droplet and the ports do the routing.

## [v0.6.2] - 2026-07-17

> The Storage step is now a plain, blank NFS form. No values are pulled from or suggested by your cluster's storage provisioners — you type your server and directory, test, move in.

### Changed

- **No more provisioner-derived suggestions in the Storage step** — v0.6.1 prefilled the share from an existing NFS-subdir provisioner when one was found. That guess is only ever right on clusters shaped like the one it was written on — so it's gone. The Storage step is now two empty fields with plain placeholders: your NFS server and the directory ProxyCTL should live in. Test still creates a not-yet-existing directory (its parent must exist) and still gates moving in.

## [v0.6.1] - 2026-07-17

> First-write Permission denied after moving in is fixed, and the Storage step now finds your cluster's existing NFS for you — on most clusters moving in is now Test → click, no typing.

### Fixed

- **Permission denied on the first write after moving in** — Kubernetes creates a missing directory on an NFS share as root with modest permissions, while ProxyCTL runs as an unprivileged user — so the first thing written after adopting a share (the droplet SSH key) failed with 'Permission denied'. Moving in now includes a root init step that creates the app directory and hands it to ProxyCTL's user before the app starts.

### Added

- **The Storage step prefills your cluster's existing NFS** — If your cluster already runs an NFS-backed StorageClass (an nfs-subdir provisioner), the Storage step discovers the backing server and export and prefills the share as a dedicated ProxyCTL directory on that same disk. It also explains where the value came from; type over it to choose somewhere else.

### Changed

- **Test share creates the directory for you** — The probe now mounts the parent directory and lets Kubernetes materialize the final folder, so testing a path that doesn't exist yet simply creates it — no pre-emptive mkdir on the NAS. Only a missing parent still fails, with an error that says so.

## [v0.6.0] - 2026-07-17

> Setup now lives entirely in the GUI. The installer asks nothing about storage: ProxyCTL boots straight to the wizard, where you name the NFS share everything will live on, TEST it from inside the cluster, and move in with one click.

### Added

- **Storage is chosen — and tested — in the app** — A fresh install starts on deliberately temporary storage and the wizard opens on a required Storage step. Enter your NFS share and hit Test: a short-lived probe pod mounts it from inside the cluster, writes and reads a file, and failures come back with the actual mount error instead of a pod silently stuck in ContainerCreating. 'Save & move in' then restarts ProxyCTL onto the share — app data in app/, every tunnel's WireGuard keys in Keys/, one directory to find and back up. Your admin login survives the move. A cluster-storage fallback (pick a StorageClass) in the same screen keeps clusters without NFS fully supported.

### Changed

- **Shares are mounted inline — no PV, PVC, or StorageClass** — Gateways and the app now mount the NFS share directly in the pod spec instead of going through a static PersistentVolume and shared claim. Nothing cluster-scoped is created for share storage anymore, which also removes the last places a permissions error could sneak into an apply.
- **The installer no longer asks about storage** — install.sh is back to image + exposure + token; the storage question moved into the GUI where it can actually be verified. Headless installs can still pre-answer it with PROXYCTL_DATA_NFS=server:/path. Existing installs keep their current volume untouched — redeploys detect and preserve whatever storage was already adopted.

## [v0.5.0] - 2026-07-17

> Pick one place for everything: the installer can now put ProxyCTL's app data and every gateway's WireGuard keys in a single NFS directory of your choosing, and the setup wizard now opens with a Storage step that helps you keep that share locked down to your cluster.

### Fixed

- **Saving a keys share from inside the cluster no longer fails** — Share mode re-applied its PersistentVolume on every save and every apply, which requires cluster-scoped permissions ProxyCTL's ServiceAccount intentionally doesn't have — so saves that worked in local development failed with a permissions error in a real install. ProxyCTL now leaves an existing PV alone and only ensures the namespaced claim, which needs no extra permissions.

### Added

- **One NFS share for the whole install** — install.sh now asks a single storage question: give it an NFS share (server:/path) and everything ProxyCTL persists — app state, the droplet SSH key, and each tunnel's WireGuard keypair — lives in that one directory (app/ and Keys/ side by side). It's one folder to find, one folder to back up, and it survives a reinstall: the volume is a static PV with reclaimPolicy Retain. Pressing Enter keeps the classic StorageClass install exactly as before.
- **Storage is now the first setup step** — The wizard's keys step moved to the front and grew practical guidance: it shows your cluster's node IPs (NFS mounts come from the nodes, not pods), advises — never requires — restricting the export to just those IPs, and offers optional paste-ready commands that create a locked-down share. On an NFS-share install the step comes prefilled with the install's own share, with keys defaulting to Keys/ inside it.
- **New-node warnings for IP-restricted shares** — Saving a keys share records which node IPs it was exported to. When a node joins the cluster later, the wizard and the Gateway keys card show exactly which node isn't covered, an updated one-line /etc/exports entry to paste, and an acknowledge button — instead of a gateway on the new node hanging at mount time with nothing naming the cause.

## [v0.4.2] - 2026-07-17

> Updates now happen only when you ask. ProxyCTL pins itself to a fixed version, so a pod restart — even one during Setup — can no longer bump you to a new build unprompted.

### Fixed

- **ProxyCTL no longer updates itself without prompting** — Installs now pin an immutable version tag instead of a moving ':latest'. Previously the deployment re-pulled the newest published image on any restart, so an ordinary pod reschedule — a node drain, an eviction, or re-running the installer, including while you were in the Setup screens — could silently upgrade ProxyCTL to a build you never chose. Now a restart re-pulls the exact same version, and the only thing that moves you to a new release is clicking 'Update now'.

### Changed

- **'Update now' pins the specific new release** — The in-app update button used to just restart the deployment and let it grab whatever ':latest' pointed at. It now sets the container image to the exact release tag shown in the banner, so you land on precisely that version — and stay on it across restarts until you choose to update again.

## [v0.4.1] - 2026-07-17

> NFS-backed keys work with servers that don't offer NFSv4.2, and a self-inflicted 'timed out' during Apply is fixed.

### Fixed

- **Keys NFS share negotiates its version instead of requiring 4.2** — The keys share volume pinned nfsvers=4.2. On a NAS that only offers NFSv3 the mount failed and the gateway pod hung before its init containers ran — surfacing as an unexplained Apply timeout rather than a storage error. The version is no longer pinned, so the mount negotiates the best both ends support (4.2 → 4.1 → 4.0 → 3). If your server already speaks 4.2 nothing changes.
- **Apply no longer cancels its own rollout wait** — The gateway rollout-status check ran with a longer timeout than the step that contained it, so ProxyCTL's own deadline cancelled kubectl and printed a bare 'timed out' that hid the real reason. The wait now sits inside a larger budget, so the actual rollout result surfaces.

## [v0.4.0] - 2026-07-17

> You can type the exact NFS server and path your WireGuard keys live on — right in Setup — instead of being tied to a fixed storage layout.

### Added

- **Name the NFS share your gateway keys are stored on** — The Keys step (and the Gateway keys card) now accept a raw NFS server + export path, with the cluster-discovered values shown as placeholders and a live preview of where each gateway's keys will land. Previously keys could only target a StorageClass that assumed specific paths, so a remote operator's keys effectively weren't stored where they expected. The per-gateway subfolder matches the provisioner's own naming, so pointing at an export that already holds keys reuses them rather than re-keying every tunnel.

## [v0.3.3] - 2026-07-17

> ProxyCTL works out where your keys should live by reading the cluster, instead of assuming a hardcoded path.

### Fixed

- **Keys StorageClass + NFS location are discovered from the cluster** — The keys location was derived from a path baked into the app, which was only correct on the maintainer's own cluster. ProxyCTL now inspects the configured StorageClass → its provisioner → that provisioner's NFS server and path to show the real location your keys will use, and falls back gracefully when it can't discover one.

## [v0.3.2] - 2026-07-17

> Setting up keys on a droplet works when you log in as a non-root SSH user (e.g. 'ubuntu' on OVH Cloud), by elevating with sudo.

### Fixed

- **Remote key setup elevates via sudo for non-root SSH users** — Applying to a droplet whose SSH user isn't root — such as the default 'ubuntu' on OVH Cloud — failed to write the WireGuard lock and keys because the commands ran unprivileged. ProxyCTL now wraps remote commands in 'sudo -n' when the login user isn't root, so a passwordless-sudo host behaves the same as a root login. The Test step reports whether it has root, sudo, or neither.

## [v0.3.1] - 2026-05-31

> Per-gateway WireGuard keys are tidied under a single folder on the NFS share (default ProxyCTL/Keys/) instead of scattering one folder per tunnel at the share root — and you can now choose that folder, and move existing keys into it, from the admin UI.

### Added

- **Choose (and move) the gateway-keys folder — in Setup and the admin UI** — The first-run Setup wizard now has a 'Keys' step where you state the NFS folder gateway keypairs are stored under, and a 'Gateway keys' card on the main screen lets you change it any time. ProxyCTL creates the matching StorageClass on save so new gateways land there. If existing gateways are still on a different folder, a 'Move existing gateways' button re-creates them under the new folder — regenerating each key and re-registering it on the droplet live (brief per-tunnel blip). Needs a small cluster RBAC grant (storageclasses: get/list/watch/create), included in the manifest.

### Changed

- **WireGuard gateway keys nest under one folder on the share** — Each gateway's keypair PVC used to be created as a top-level folder on the NFS SSD (e.g. proxyctl-wg-gw-cs2-keys-pvc-<uuid>), cluttering the share root with one entry per tunnel — plus orphaned leftovers, since the default class never reclaimed deleted dirs. Gateways now use a dedicated StorageClass whose pathPattern nests them under the chosen folder (default ProxyCTL/Keys/), and which reclaims the dir when a gateway is removed so stale folders no longer accumulate.

## [v0.3.0] - 2026-05-29

> ProxyCTL now shows what's new and lets you check for updates from inside the app — the same experience as GameCTL.

### Added

- **'What's new' panel + 'Check for updates' button in the header** — The header now shows the running version and a one-click 'Check for updates' (forces a fresh check, bypassing the 30-min cache). 'What's new' opens an in-app changelog listing every release with colour-coded Fix / New / Changed / Removed / Security tags, highlighting the build you're on.

### Changed

- **Update banner opens the in-app changelog** — When an update is available the banner's 'What's changing' button opens the same in-app notes (scrolled to the new version) instead of only linking out to GitHub. 'Later' dismisses it for that version; a manual check brings it back.

## [v0.2.2] - 2026-05-29

> Installer is now truly one-line: it no longer prompts for the container image and works under `curl … | bash` on a fresh box.

### Fixed

- **Installer fetches the manifest correctly under curl | bash** — A piped install has no script file on disk, so the old $0-based path resolved to /home and the install died with 'sed: can't read /home/k8s/proxyctl.yaml'. The installer now resolves the manifest in order — an explicit $PROXYCTL_MANIFEST, then a bundled checkout file, then the published raw URL — so `curl -fsSL https://proxyctl.cc/install.sh | bash` Just Works.

### Changed

- **No more 'Container image' prompt — pulls the published image automatically** — The installer used to stop and ask which image to pull on every run. It now uses the published GHCR image automatically; set PROXYCTL_IMAGE=… beforehand to point at a private registry without any prompt.

## [v0.2.1] - 2026-05-29

> Public installer points at the real published image instead of a placeholder.

### Fixed

- **Installer defaults to the published GHCR image** — The public install.sh previously defaulted to a scrubbed placeholder registry, so a fresh install couldn't pull ProxyCTL. It now defaults to the public GHCR image the cluster can pull anonymously.

## [v0.2.0] - 2026-05-29

> ProxyCTL can now tell you when a new version ships and update itself in one click, wears its orange brand, and is covered by tests that gate every deploy.

### Added

- **In-app update notifications + one-click self-update** — ProxyCTL polls its public GitHub releases and shows a banner when a newer version is out. 'Update now' does a rolling restart of ProxyCTL's own Deployment so it re-pulls the latest image — the auth Secret, droplet config, and every gateway are left intact, so there's no re-setup.
- **Go unit tests + CI, gating cluster deploys** — Added NAT/DNAT render and JWT auth round-trip tests plus a GitHub CI workflow (build / vet / test / UI build). clusterdeploy now runs `go vet` + `go test` and refuses to deploy if they fail.

### Changed

- **Orange brand + matching app theme** — New ProxyCTL logo and droplet header mark, and the app theme moved from emerald to the orange brand palette (green is kept for success states only).

## [v0.1.0] - 2026-05-29

> First public packaging of ProxyCTL: a clean repo layout and a scrubbed public mirror with a one-line installer.

### Added

- **One-line installer + published container image** — scripts/install.sh deploys ProxyCTL into a cluster (detects ingress / MetalLB and picks how to expose the UI), pulling a published container image.

### Changed

- **Reorganised into server/ with a public-mirror sync** — Code moved under server/, runtime state and credential files are untracked, and a sync script publishes a scrubbed, single-commit public mirror (no homelab IPs, hostnames, or keys).

## [v0.0.1-beta] - 2026-05-26

> Initial public beta: drive WireGuard ↔ Kubernetes proxying and Cloudflare DNS from one small admin UI.

### Added

- **Centralised WireGuard gateways targeting any namespace** — Gateways live in the ProxyCTL namespace and can proxy to a Service in any namespace — one app drives them all. L4 port/game tunnels DNAT traffic from a droplet into the cluster.
- **Web/Ingress tunnels via Traefik with real TLS** — Host-routed web apps are exposed through Traefik with automatic Let's Encrypt certificates, with a Cloudflare Tunnel option and one-click DNS record creation.

### Security

- **Input validation hardening** — Entry fields are validated (TargetIP must be an IP, no control characters) before they reach the cluster.
