%global goipath github.com/ruthlesslypractical/hippocampus
%global version 2.0.2
%global release 1%{?dist}

Name:           hippocampus
Version:        %{version}
Release:        %{release}
Summary:        Persistent AI Memory System with LLM-powered analysis

License:        BSD-3-Clause
URL:            https://%{goipath}
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.21
BuildRequires:  git
BuildRequires:  make

# Redis/Valkey is a runtime dependency, not bundled
Requires:       redis >= 6.0

# Don't strip Go binaries (breaks them)
%global __strip /bin/true
# Don't generate debug packages for Go
%global debug_package %{nil}

%description
Hippocampus is a persistent memory system for LLM-based AI assistants.
It provides automatic conversation capture, semantic search, epistemic
fact-checking, track classification, and associative link discovery.

Components:
- hippocampus-hook: Synchronous per-message capture and recall injection
- hippocampus-daemon: Background processor (classify, extract, verify, link)
- hippocampus-summarize: Fractal summarization (3h/daily/weekly/cross-track)
- hippocampus-mcp: MCP server (14 tools for memory management)
- hippocampus-slack: Slack channel archival bot

%prep
%autosetup -n %{name}-%{version}

%build
export GOFLAGS="-trimpath"
export LDFLAGS="-s -w -X '%{goipath}/internal/config.Version=%{version}'"

go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o bin/hippocampus-mcp ./cmd/mcp-server/
go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o bin/hippocampus-hook ./cmd/hook/
go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o bin/hippocampus-daemon ./cmd/daemon/
go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o bin/hippocampus-summarize ./cmd/summarize/
go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o bin/hippocampus-slack ./cmd/slack/
go build ${GOFLAGS} -ldflags "${LDFLAGS}" -o bin/hippocampus-admin ./cmd/admin/

%install
install -d %{buildroot}%{_bindir}
install -d %{buildroot}%{_sysconfdir}/hippocampus
install -d %{buildroot}%{_unitdir}
install -d %{buildroot}%{_docdir}/%{name}

# Binaries
install -m 755 bin/hippocampus-mcp %{buildroot}%{_bindir}/
install -m 755 bin/hippocampus-hook %{buildroot}%{_bindir}/
install -m 755 bin/hippocampus-daemon %{buildroot}%{_bindir}/
install -m 755 bin/hippocampus-summarize %{buildroot}%{_bindir}/
install -m 755 bin/hippocampus-slack %{buildroot}%{_bindir}/
install -m 755 bin/hippocampus-admin %{buildroot}%{_bindir}/

# Config
install -m 644 docs/config-reference.json %{buildroot}%{_sysconfdir}/hippocampus/config.json

# Systemd units
cat > %{buildroot}%{_unitdir}/hippocampus-daemon.service << 'EOF'
[Unit]
Description=Hippocampus Background Daemon
After=network.target redis.service
Wants=redis.service

[Service]
Type=simple
ExecStart=%{_bindir}/hippocampus-daemon
Environment=HIPPOCAMPUS_CONFIG=%{_sysconfdir}/hippocampus/config.json
Restart=on-failure
RestartSec=5
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF

cat > %{buildroot}%{_unitdir}/hippocampus-slack.service << 'EOF'
[Unit]
Description=Hippocampus Slack Channel Archival Bot
After=network.target redis.service hippocampus-daemon.service
Wants=redis.service

[Service]
Type=simple
ExecStart=%{_bindir}/hippocampus-slack
Environment=HIPPOCAMPUS_CONFIG=%{_sysconfdir}/hippocampus/config.json
Restart=on-failure
RestartSec=10
Nice=12
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF

cat > %{buildroot}%{_unitdir}/hippocampus-summarize.timer << 'EOF'
[Unit]
Description=Hippocampus Summarization Timer (every 3h)

[Timer]
OnBootSec=15min
OnUnitActiveSec=3h
Persistent=true

[Install]
WantedBy=timers.target
EOF

cat > %{buildroot}%{_unitdir}/hippocampus-summarize.service << 'EOF'
[Unit]
Description=Hippocampus Fractal Summarization
After=network.target redis.service

[Service]
Type=oneshot
ExecStart=%{_bindir}/hippocampus-summarize --3h
Environment=HIPPOCAMPUS_CONFIG=%{_sysconfdir}/hippocampus/config.json
Nice=15
IOSchedulingClass=idle
EOF

# Docs
install -m 644 README.md %{buildroot}%{_docdir}/%{name}/ 2>/dev/null || true
install -m 644 docs/config-reference.json %{buildroot}%{_docdir}/%{name}/

# Cron (alternative to systemd timer)
install -d %{buildroot}%{_sysconfdir}/cron.d
cat > %{buildroot}%{_sysconfdir}/cron.d/hippocampus << 'EOF'
# Hippocampus fractal summarization — runs every 3 hours
# Uncomment to use cron instead of hippocampus-summarize.timer
#0 */3 * * * root HIPPOCAMPUS_CONFIG=/etc/hippocampus/config.json /usr/bin/hippocampus-summarize --3h >> /var/log/hippocampus/summarize.log 2>&1

# Daily full summary rollup (4:15 AM)
#15 4 * * * root HIPPOCAMPUS_CONFIG=/etc/hippocampus/config.json /usr/bin/hippocampus-summarize --daily >> /var/log/hippocampus/summarize.log 2>&1

# Weekly summary (Sunday 5:00 AM)
#0 5 * * 0 root HIPPOCAMPUS_CONFIG=/etc/hippocampus/config.json /usr/bin/hippocampus-summarize --weekly >> /var/log/hippocampus/summarize.log 2>&1
EOF

%files
%{_bindir}/hippocampus-mcp
%{_bindir}/hippocampus-hook
%{_bindir}/hippocampus-daemon
%{_bindir}/hippocampus-summarize
%{_bindir}/hippocampus-slack
%{_bindir}/hippocampus-admin
%config(noreplace) %{_sysconfdir}/hippocampus/config.json
%config(noreplace) %{_sysconfdir}/cron.d/hippocampus
%{_unitdir}/hippocampus-daemon.service
%{_unitdir}/hippocampus-slack.service
%{_unitdir}/hippocampus-summarize.timer
%{_unitdir}/hippocampus-summarize.service
%{_docdir}/%{name}/

%post
%systemd_post hippocampus-daemon.service
%systemd_post hippocampus-slack.service
%systemd_post hippocampus-summarize.timer

%preun
%systemd_preun hippocampus-daemon.service
%systemd_preun hippocampus-slack.service
%systemd_preun hippocampus-summarize.timer

%postun
%systemd_postun_with_restart hippocampus-daemon.service
%systemd_postun_with_restart hippocampus-slack.service
%systemd_postun hippocampus-summarize.timer

%changelog
* Tue Jul 29 2026 Theron Bair <tbair@ruthlesslypractical.com> - 2.0.2-1
- Add hippocampus-slack.service systemd unit (was missing from packaging)
- Add hippocampus-admin to RPM (was missing from packaging)
- Several other fixes that are too shameful to mention

* Fri Jul 25 2026 Theron Bair <tbair@ruthlesslypractical.com> - 2.0.0-1
- Recall v2: tiered injection (full/summary/breadcrumb), track weighting, relevance floor
- Track orientation system with auto-injection on track shift
- hippocampus-admin CLI (entry, tag, orientation, summary management)
- Condenser: per-entry LLM summaries for compact Tier 2 recall
- Discovery linker: random-pair cross-track link evaluation
- Link quality: content length gate, graded scoring, same-session protection
- Working set tracker sidecar (qwen3:1.7b)
- OFC neuromodulator (DA/5HT sentiment feedback)
- Daemon self-update detection (binary mtime, clean restart)
- 16 magic numbers extracted to config tunables
- Debian packaging (Ubuntu 25.10)
- BSD-3-Clause license headers on all source files
- Full docs update (README, OVERVIEW, TUNABLES, TESTING)

* Wed Jul 23 2026 Theron Bair <tbair@ruthlesslypractical.com> - 1.0.2-1
- Add hippocampus-daemon (priority dispatcher with GPU concurrency control)
- Epistemic pipeline: extraction, verification, Simon Says verb filter
- Structured logging (slog) across all binaries
- Per-subsystem Ollama routing
- Co-recall and temporal neighbor linking
- Removed pkg/consolidate (moved into daemon)
- Refactored: internal/util, RedisConfig.NewRedisClient()

* Mon Jul 21 2026 Theron Bair <tbair@ruthlesslypractical.com> - 1.0.1-1
- Initial SRPM packaging
