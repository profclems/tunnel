        // State
        let tunnelsList = [];
        let filteredTunnels = [];
        let filterQuery = '';
        let collapsedGroups = {};
        let requestsMap = {};
        let requestsList = [];
        let filteredRequestsList = [];
        let selectedId = null;
        let sseConnected = false;
        let metricsCollapsed = false;
        let metricsData = null;
        let selectedForDiff = [];
        let currentDiffTab = 'url';
        let reconnectCount = 0;
        let lastHeartbeat = null;
        let connectionStartTime = null;
        let notificationsEnabled = false;
        let isPaused = false;
        let pausedQueue = [];
        let theme = localStorage.getItem('tunnel_theme') || 'dark';
        let pinnedRequests = new Set();
        let annotations = {};
        let groupingEnabled = false;

        // ===== DEFENSIVE HELPERS =====
        // Safe element access for custom templates that may not have all elements
        function $(id) {
            return document.getElementById(id);
        }

        function setText(id, text) {
            const el = $(id);
            if (el) el.textContent = text;
        }

        function setDisplay(id, display) {
            const el = $(id);
            if (el) el.style.display = display;
        }

        function addClass(id, cls) {
            const el = $(id);
            if (el) el.classList.add(cls);
        }

        function removeClass(id, cls) {
            const el = $(id);
            if (el) el.classList.remove(cls);
        }

        function toggleClass(id, cls, force) {
            const el = $(id);
            if (el) el.classList.toggle(cls, force);
        }

        // ===== METRICS =====

        function fetchMetrics() {
            fetch('/api/metrics')
                .then(r => r.json())
                .then(data => {
                    metricsData = data;
                    updateMetricsUI();
                })
                .catch(err => console.error('Failed to fetch metrics:', err));
        }

        function updateMetricsUI() {
            if (!metricsData) return;

            setText('bytes-in', formatBytes(metricsData.total_bytes_in));
            setText('bytes-out', formatBytes(metricsData.total_bytes_out));
            setText('req-rate', Math.round(metricsData.requests_per_min));
            setText('error-count', metricsData.error_count);

            // Update percentiles
            setText('p50-latency', metricsData.p50_latency_ms || '--');
            setText('p95-latency', metricsData.p95_latency_ms || '--');
            setText('p99-latency', metricsData.p99_latency_ms || '--');

            renderLatencyChart(metricsData.latency_history || []);
            renderRateChart(metricsData.rate_history || []);
            renderErrorBreakdown(metricsData.errors_by_status || {});
            renderHistogram(metricsData.latency_buckets || []);
        }

        function renderErrorBreakdown(errorsByStatus) {
            const container = $('error-bars');
            const section = $('error-breakdown');
            if (!container || !section) return;

            const entries = Object.entries(errorsByStatus);
            if (entries.length === 0) {
                section.style.display = 'none';
                return;
            }

            section.style.display = 'block';
            const maxCount = Math.max(...Object.values(errorsByStatus), 1);

            let html = '';
            entries.sort((a, b) => b[1] - a[1]).forEach(([status, count]) => {
                const pct = (count / maxCount) * 100;
                const statusClass = parseInt(status) >= 500 ? 's-5xx' : 's-4xx';
                html += '<div class="error-bar-item" onclick="filterByStatus(' + status + ')">';
                html += '<span class="error-bar-label">' + status + '</span>';
                html += '<div class="error-bar-track"><div class="error-bar-fill ' + statusClass + '" style="width:' + pct + '%"></div></div>';
                html += '<span class="error-bar-count">' + count + '</span>';
                html += '</div>';
            });

            container.innerHTML = html;
        }

        function toggleErrorBreakdown() {
            const body = document.getElementById('error-breakdown-body');
            const chevron = document.getElementById('error-chevron');
            if (body.style.display === 'none') {
                body.style.display = 'block';
                chevron.textContent = '▼';
            } else {
                body.style.display = 'none';
                chevron.textContent = '▶';
            }
        }

        const histogramLabels = ['<10ms', '10-25ms', '25-50ms', '50-100ms', '100-250ms', '250-500ms', '0.5-1s', '1-2.5s', '2.5-5s', '>5s'];

        function renderHistogram(buckets) {
            const container = document.getElementById('histogram-bars');
            if (!buckets || buckets.length === 0) {
                container.innerHTML = '<div style="color: var(--text-muted); font-size: 10px;">No data</div>';
                return;
            }

            const maxCount = Math.max(...buckets, 1);

            let html = '';
            buckets.forEach((count, idx) => {
                const pct = (count / maxCount) * 100;
                // Color: green for fast, yellow for medium, red for slow
                let colorClass = 'fast';
                if (idx >= 4 && idx < 7) colorClass = 'medium';
                else if (idx >= 7) colorClass = 'slow';

                html += '<div class="histogram-bar-item">';
                html += '<span class="histogram-bar-label">' + histogramLabels[idx] + '</span>';
                html += '<div class="histogram-bar-track"><div class="histogram-bar-fill ' + colorClass + '" style="width:' + pct + '%"></div></div>';
                html += '<span class="histogram-bar-count">' + count + '</span>';
                html += '</div>';
            });

            container.innerHTML = html;
        }

        function toggleHistogram() {
            const body = document.getElementById('histogram-body');
            const chevron = document.getElementById('histogram-chevron');
            body.classList.toggle('collapsed');
            chevron.textContent = body.classList.contains('collapsed') ? '▶' : '▼';
        }

        function filterByStatus(status) {
            // Set the status filter dropdown
            const statusStr = status.toString();
            const statusFilter = document.getElementById('status-filter');
            if (status >= 500) {
                statusFilter.value = '5xx';
            } else if (status >= 400) {
                statusFilter.value = '4xx';
            }
            // Also set specific status in path filter for exact match
            document.getElementById('req-filter-input').value = statusStr;
            filterRequests();
            showCopyToast('Filtering: ' + status);
        }

        function renderRateChart(rates) {
            const svg = document.getElementById('rate-svg');
            if (!svg) return;

            const w = svg.clientWidth || 180;
            const h = svg.clientHeight || 40;
            const padding = 2;

            // Use last 30 seconds for the bar chart
            const data = rates.slice(-30);
            if (data.length === 0 || data.every(r => r === 0)) {
                svg.innerHTML = '<text x="50%" y="50%" text-anchor="middle" fill="#6e7681" font-size="9">No traffic</text>';
                return;
            }

            const maxRate = Math.max(...data, 1);
            const barWidth = (w - padding * 2) / data.length - 1;

            let bars = '';
            data.forEach((rate, i) => {
                const x = padding + i * (barWidth + 1);
                const barHeight = (rate / maxRate) * (h - padding * 2);
                const y = h - padding - barHeight;
                bars += '<rect class="rate-bar" x="' + x + '" y="' + y + '" width="' + barWidth + '" height="' + barHeight + '" rx="1"/>';
            });

            const currentRate = data[data.length - 1] || 0;
            svg.innerHTML = bars + '<text x="' + (w - padding) + '" y="10" text-anchor="end" fill="var(--text-muted)" font-size="9">' + currentRate + '/s</text>';
        }

        function renderLatencyChart(points) {
            const svg = document.getElementById('latency-svg');
            if (!svg || points.length < 2) {
                if (svg) svg.innerHTML = '<text x="50%" y="50%" text-anchor="middle" fill="#6e7681" font-size="10">Awaiting data...</text>';
                return;
            }

            const w = svg.clientWidth || 380;
            const h = svg.clientHeight || 50;
            const padding = 4;

            const maxLatency = Math.max(...points.map(p => p.ms), 1);
            const xScale = (w - padding * 2) / (points.length - 1);
            const yScale = (h - padding * 2) / maxLatency;

            const pathData = points.map((p, i) => {
                const x = padding + i * xScale;
                const y = h - padding - p.ms * yScale;
                return (i === 0 ? 'M' : 'L') + x.toFixed(1) + ',' + y.toFixed(1);
            }).join(' ');

            const avgLatency = points.reduce((sum, p) => sum + p.ms, 0) / points.length;

            svg.innerHTML =
                '<path d="' + pathData + '" fill="none" stroke="var(--primary)" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>' +
                '<text x="' + (w - padding) + '" y="12" text-anchor="end" fill="var(--text-muted)" font-size="9">avg ' + Math.round(avgLatency) + 'ms</text>';
        }

        function toggleMetricsPanel() {
            metricsCollapsed = !metricsCollapsed;
            const body = document.getElementById('metrics-body');
            const chevron = document.getElementById('metrics-chevron');
            if (metricsCollapsed) {
                body.style.display = 'none';
                chevron.style.transform = 'rotate(-90deg)';
            } else {
                body.style.display = 'block';
                chevron.style.transform = 'rotate(0deg)';
            }
        }

        function formatBytes(bytes) {
            if (bytes === 0) return '0 B';
            const k = 1024;
            const sizes = ['B', 'KB', 'MB', 'GB'];
            const i = Math.floor(Math.log(bytes) / Math.log(k));
            return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
        }

        function formatDuration(ms) {
            if (ms === null || ms === undefined) return '--';
            if (ms < 0.001) {
                // Nanoseconds
                return Math.round(ms * 1000000) + 'ns';
            } else if (ms < 1) {
                // Microseconds
                return Math.round(ms * 1000) + 'µs';
            } else if (ms < 1000) {
                // Milliseconds
                return ms < 10 ? ms.toFixed(1) + 'ms' : Math.round(ms) + 'ms';
            } else if (ms < 60000) {
                // Seconds
                return (ms / 1000).toFixed(2) + 's';
            } else {
                // Minutes and seconds
                const mins = Math.floor(ms / 60000);
                const secs = ((ms % 60000) / 1000).toFixed(0);
                return mins + 'm ' + secs + 's';
            }
        }

        // Start metrics polling
        setInterval(fetchMetrics, 2000);

        // ===== REQUEST FILTERING =====

        function filterRequests() {
            const pathFilter = document.getElementById('req-filter-input').value.toLowerCase();
            const methodFilter = document.getElementById('method-filter').value;
            const statusFilter = document.getElementById('status-filter').value;

            filteredRequestsList = requestsList.filter(req => {
                // Path filter
                if (pathFilter && !req.path.toLowerCase().includes(pathFilter)) {
                    return false;
                }
                // Method filter
                if (methodFilter && req.method !== methodFilter) {
                    return false;
                }
                // Status filter
                if (statusFilter) {
                    const status = req.status;
                    if (statusFilter === '2xx' && (status < 200 || status >= 300)) return false;
                    if (statusFilter === '4xx' && (status < 400 || status >= 500)) return false;
                    if (statusFilter === '5xx' && (status < 500 || status >= 600)) return false;
                }
                return true;
            });

            renderFilteredRequests();
        }

        function renderFilteredRequests() {
            const container = document.getElementById('req-list');
            container.innerHTML = '';

            if (groupingEnabled) {
                renderGroupedRequests(container);
            } else {
                filteredRequestsList.forEach(req => {
                    container.appendChild(createRequestItem(req));
                });
            }
        }

        function renderGroupedRequests(container) {
            // Group requests by path prefix (first 2 segments)
            const groups = {};

            filteredRequestsList.forEach(req => {
                const prefix = getPathPrefix(req.path);
                if (!groups[prefix]) {
                    groups[prefix] = { requests: [], errorCount: 0, totalLatency: 0 };
                }
                groups[prefix].requests.push(req);
                if (req.status >= 400) groups[prefix].errorCount++;
                groups[prefix].totalLatency += req.duration_ms || 0;
            });

            // Sort groups by most requests first
            const sortedGroups = Object.entries(groups).sort((a, b) => b[1].requests.length - a[1].requests.length);

            sortedGroups.forEach(([prefix, group]) => {
                const isCollapsed = collapsedGroups[prefix] === true;
                const avgLatency = group.requests.length > 0 ? Math.round(group.totalLatency / group.requests.length) : 0;

                // Create group header
                const header = document.createElement('div');
                header.className = 'request-group-header';
                header.onclick = () => toggleGroup(prefix);
                header.innerHTML = `
                    <div class="request-group-title">
                        <span class="request-group-chevron">${isCollapsed ? '▶' : '▼'}</span>
                        <span>${prefix}</span>
                    </div>
                    <div class="request-group-stats">
                        <span>${group.requests.length} requests</span>
                        <span>avg ${avgLatency}ms</span>
                        ${group.errorCount > 0 ? '<span class="stat-errors">' + group.errorCount + ' errors</span>' : ''}
                    </div>
                `;
                container.appendChild(header);

                // Create group content
                const content = document.createElement('div');
                content.className = 'request-group-content' + (isCollapsed ? ' collapsed' : '');
                content.id = 'group-' + encodeURIComponent(prefix);
                group.requests.forEach(req => {
                    content.appendChild(createRequestItem(req));
                });
                container.appendChild(content);
            });
        }

        function getPathPrefix(path) {
            const segments = path.split('/').filter(s => s);
            if (segments.length === 0) return '/';
            if (segments.length === 1) return '/' + segments[0];
            return '/' + segments.slice(0, 2).join('/');
        }

        function toggleGroup(prefix) {
            collapsedGroups[prefix] = !collapsedGroups[prefix];
            renderFilteredRequests();
        }

        function toggleGrouping() {
            groupingEnabled = !groupingEnabled;
            const btn = document.getElementById('btn-group-toggle');
            btn.classList.toggle('active', groupingEnabled);
            renderFilteredRequests();
            showCopyToast(groupingEnabled ? 'Grouping enabled' : 'Grouping disabled');
        }

        // ===== WEBSOCKET PANEL =====
        let wsMessages = [];
        let wsPanelVisible = false;

        function toggleWSPanel() {
            wsPanelVisible = !wsPanelVisible;
            const panel = document.getElementById('ws-panel');
            const btn = document.getElementById('btn-ws-toggle');

            if (wsPanelVisible) {
                panel.style.display = 'block';
                btn.classList.add('active');
                refreshWSMessages();
            } else {
                panel.style.display = 'none';
                btn.classList.remove('active');
            }
        }

        function refreshWSMessages() {
            fetch('/api/ws-messages')
                .then(r => r.json())
                .then(data => {
                    wsMessages = data || [];
                    renderWSMessages();
                    updateWSCount();
                })
                .catch(err => console.error('Failed to load WS messages:', err));
        }

        function clearWSMessages() {
            fetch('/api/ws-messages/clear', { method: 'POST' })
                .then(() => {
                    wsMessages = [];
                    renderWSMessages();
                    updateWSCount();
                    showCopyToast('WebSocket messages cleared');
                })
                .catch(err => console.error('Failed to clear WS messages:', err));
        }

        function updateWSCount() {
            document.getElementById('ws-count').textContent = wsMessages.length;
        }

        function renderWSMessages() {
            const container = document.getElementById('ws-messages-list');

            if (wsMessages.length === 0) {
                container.innerHTML = '<div class="ws-empty">No WebSocket messages captured yet</div>';
                return;
            }

            let html = '';
            // Show newest first
            const reversed = [...wsMessages].reverse();
            reversed.forEach(msg => {
                const dirIcon = msg.direction === 'in' ? '⬇️' : '⬆️';
                const dirClass = msg.direction;
                const time = new Date(msg.timestamp).toLocaleTimeString();
                const dataPreview = msg.data.length > 80 ? msg.data.substring(0, 80) + '...' : msg.data;

                html += '<div class="ws-message-item" onclick="showWSMessageDetail(\'' + msg.id + '\')">';
                html += '<span class="ws-direction ' + dirClass + '">' + dirIcon + '</span>';
                html += '<div class="ws-message-content">';
                html += '<div class="ws-message-data">' + escapeHtml(dataPreview) + '</div>';
                html += '<div class="ws-message-meta">';
                html += '<span>' + msg.message_type + '</span>';
                html += '<span>' + formatBytes(msg.size) + '</span>';
                html += '<span>' + time + '</span>';
                html += '</div>';
                html += '</div>';
                html += '</div>';
            });

            container.innerHTML = html;
        }

        function showWSMessageDetail(msgId) {
            const msg = wsMessages.find(m => m.id === msgId);
            if (!msg) return;

            closeDynamicModals();
            const modal = document.createElement('div');
            modal.className = 'modal-overlay visible';
            modal.setAttribute('data-dynamic', 'true');
            modal.innerHTML = `
                <div class="modal-content" style="width: 600px; max-height: 80vh;">
                    <div class="modal-header">
                        <h3>${msg.direction === 'in' ? '⬇️ Incoming' : '⬆️ Outgoing'} Message</h3>
                        <button class="modal-close" onclick="this.closest('.modal-overlay').remove()">&times;</button>
                    </div>
                    <div class="modal-body" style="overflow: auto;">
                        <div style="margin-bottom: 12px; font-size: 12px; color: var(--text-muted);">
                            Type: ${msg.message_type} | Size: ${formatBytes(msg.size)} | Time: ${new Date(msg.timestamp).toLocaleString()}
                        </div>
                        <pre style="background: var(--bg-tertiary); padding: 12px; border-radius: 4px; overflow: auto; max-height: 400px; font-size: 12px;">${escapeHtml(msg.data)}</pre>
                    </div>
                </div>
            `;
            document.body.appendChild(modal);
            modal.onclick = (e) => {
                if (e.target === modal) modal.remove();
            };
        }

        // Poll for WS messages when panel is visible
        setInterval(() => {
            if (wsPanelVisible) {
                refreshWSMessages();
            }
        }, 2000);

        // ===== SYNTAX HIGHLIGHTING =====

        function highlightJSON(str) {
            try {
                const obj = JSON.parse(str);
                str = JSON.stringify(obj, null, 2);
            } catch (e) {
                // Not valid JSON, return as-is
                return escapeHtml(str);
            }
            return escapeHtml(str)
                .replace(/"([^"]+)":/g, '<span class="json-key">"$1"</span>:')
                .replace(/: "([^"]*)"/g, ': <span class="json-string">"$1"</span>')
                .replace(/: (-?\d+\.?\d*)/g, ': <span class="json-number">$1</span>')
                .replace(/: (true|false)/g, ': <span class="json-bool">$1</span>')
                .replace(/: (null)/g, ': <span class="json-null">$1</span>');
        }

        function formatBody(body, contentType) {
            if (!body) return '<span style="color: var(--text-muted)">No content</span>';
            contentType = contentType || '';
            if (contentType.includes('json') || body.trim().startsWith('{') || body.trim().startsWith('[')) {
                return highlightJSON(body);
            }
            return escapeHtml(body);
        }

        // ===== EXPORT FUNCTIONS =====

        function toggleExportMenu() {
            const menu = document.getElementById('export-menu');
            menu.classList.toggle('visible');

            // Close menu when clicking outside
            if (menu.classList.contains('visible')) {
                setTimeout(() => {
                    document.addEventListener('click', closeExportMenu);
                }, 0);
            }
        }

        function closeExportMenu(e) {
            const menu = document.getElementById('export-menu');
            const dropdown = e.target.closest('.export-dropdown');
            if (!dropdown) {
                menu.classList.remove('visible');
                document.removeEventListener('click', closeExportMenu);
            }
        }

        function generateCurl(r) {
            let cmd = 'curl';

            // Method
            if (r.method !== 'GET') {
                cmd += ' -X ' + r.method;
            }

            // Headers
            if (r.req_header) {
                for (const [key, values] of Object.entries(r.req_header)) {
                    if (key.toLowerCase() === 'host') continue;
                    const val = Array.isArray(values) ? values[0] : values;
                    cmd += " \\\n  -H '" + key + ': ' + val.replace(/'/g, "'\\''") + "'";
                }
            }

            // Body
            if (r.req_body) {
                cmd += " \\\n  -d '" + r.req_body.replace(/'/g, "'\\''") + "'";
            }

            // URL
            cmd += " \\\n  '" + r.url + "'";

            return cmd;
        }

        function generateFetch(r) {
            const options = {
                method: r.method
            };

            // Headers
            if (r.req_header) {
                options.headers = {};
                for (const [key, values] of Object.entries(r.req_header)) {
                    if (key.toLowerCase() === 'host') continue;
                    options.headers[key] = Array.isArray(values) ? values[0] : values;
                }
            }

            // Body
            if (r.req_body) {
                options.body = r.req_body;
            }

            return "fetch('" + r.url + "', " + JSON.stringify(options, null, 2) + ")\n  .then(res => res.json())\n  .then(console.log)\n  .catch(console.error);";
        }

        function copyAsCurl() {
            if (!selectedId) return;
            const r = requestsMap[selectedId];
            if (!r) return;

            const curl = generateCurl(r);
            copyToClipboard(curl);
            document.getElementById('export-menu').classList.remove('visible');
        }

        function copyAsFetch() {
            if (!selectedId) return;
            const r = requestsMap[selectedId];
            if (!r) return;

            const fetchCode = generateFetch(r);
            copyToClipboard(fetchCode);
            document.getElementById('export-menu').classList.remove('visible');
        }

        function copyToClipboard(text) {
            navigator.clipboard.writeText(text).then(() => {
                showCopyToast('Copied to clipboard!');
            }).catch(err => {
                console.error('Failed to copy:', err);
            });
        }

        function showCopyToast(message) {
            let toast = document.getElementById('copy-toast');
            if (!toast) {
                toast = document.createElement('div');
                toast.id = 'copy-toast';
                toast.className = 'copy-toast';
                document.body.appendChild(toast);
            }
            toast.textContent = message;
            toast.classList.add('visible');
            setTimeout(() => {
                toast.classList.remove('visible');
            }, 2000);
        }

        // ===== DIFF COMPARISON =====

        function toggleDiffSelection(id, checked) {
            if (checked) {
                if (selectedForDiff.length < 2) {
                    selectedForDiff.push(id);
                } else {
                    // Already have 2, replace the oldest
                    selectedForDiff.shift();
                    selectedForDiff.push(id);
                    // Update checkboxes in UI
                    renderRequests();
                }
            } else {
                selectedForDiff = selectedForDiff.filter(x => x !== id);
            }
            updateCompareBar();
        }

        function updateCompareBar() {
            const bar = document.getElementById('compare-bar');
            const countEl = document.getElementById('compare-count');
            const btnCompare = document.getElementById('btn-compare');

            countEl.textContent = selectedForDiff.length;

            if (selectedForDiff.length > 0) {
                bar.classList.add('visible');
            } else {
                bar.classList.remove('visible');
            }

            btnCompare.disabled = selectedForDiff.length !== 2;
            btnCompare.style.opacity = selectedForDiff.length === 2 ? '1' : '0.5';
        }

        function clearDiffSelection() {
            selectedForDiff = [];
            updateCompareBar();
            renderRequests();
        }

        function showDiffModal() {
            if (selectedForDiff.length !== 2) return;

            const modal = document.getElementById('diff-modal');
            modal.classList.add('visible');
            currentDiffTab = 'url';
            updateDiffTabs();
            renderDiffContent();
        }

        function closeDiffModal(event) {
            if (event && event.target !== event.currentTarget) return;
            document.getElementById('diff-modal').classList.remove('visible');
        }

        function switchDiffTab(tab) {
            currentDiffTab = tab;
            updateDiffTabs();
            renderDiffContent();
        }

        function updateDiffTabs() {
            document.querySelectorAll('.diff-tab').forEach(el => {
                el.classList.toggle('active', el.dataset.diffTab === currentDiffTab);
            });
        }

        function renderDiffContent() {
            const req1 = requestsMap[selectedForDiff[0]];
            const req2 = requestsMap[selectedForDiff[1]];

            if (!req1 || !req2) return;

            const leftHeader = document.getElementById('diff-left-header');
            const rightHeader = document.getElementById('diff-right-header');
            const leftContent = document.getElementById('diff-left-content');
            const rightContent = document.getElementById('diff-right-content');

            leftHeader.textContent = req1.method + ' ' + req1.path + ' (' + new Date(req1.timestamp).toLocaleTimeString() + ')';
            rightHeader.textContent = req2.method + ' ' + req2.path + ' (' + new Date(req2.timestamp).toLocaleTimeString() + ')';

            let left = '', right = '';

            switch (currentDiffTab) {
                case 'url':
                    left = formatForDiff('Method: ' + req1.method + '\nURL: ' + req1.url + '\nPath: ' + req1.path + '\nStatus: ' + req1.status + '\nDuration: ' + formatDuration(req1.duration_ms));
                    right = formatForDiff('Method: ' + req2.method + '\nURL: ' + req2.url + '\nPath: ' + req2.path + '\nStatus: ' + req2.status + '\nDuration: ' + formatDuration(req2.duration_ms));
                    break;
                case 'headers':
                    left = formatForDiff(formatHeaders(req1.req_header || {}));
                    right = formatForDiff(formatHeaders(req2.req_header || {}));
                    break;
                case 'body':
                    left = formatForDiff(formatBodyForDiff(req1.req_body));
                    right = formatForDiff(formatBodyForDiff(req2.req_body));
                    break;
            }

            const { leftHtml, rightHtml } = computeDiff(left, right);
            leftContent.innerHTML = leftHtml;
            rightContent.innerHTML = rightHtml;
        }

        function formatHeaders(headers) {
            if (!headers || Object.keys(headers).length === 0) return '(no headers)';
            return Object.entries(headers)
                .map(([k, v]) => k + ': ' + (Array.isArray(v) ? v.join(', ') : v))
                .sort()
                .join('\n');
        }

        function formatBodyForDiff(body) {
            if (!body) return '(no body)';
            try {
                return JSON.stringify(JSON.parse(body), null, 2);
            } catch {
                return body;
            }
        }

        function formatForDiff(text) {
            return text.split('\n');
        }

        function computeDiff(leftLines, rightLines) {
            const maxLen = Math.max(leftLines.length, rightLines.length);
            let leftHtml = '';
            let rightHtml = '';

            for (let i = 0; i < maxLen; i++) {
                const l = leftLines[i] || '';
                const r = rightLines[i] || '';

                if (l === r) {
                    leftHtml += '<div class="diff-line unchanged">' + escapeHtml(l) + '</div>';
                    rightHtml += '<div class="diff-line unchanged">' + escapeHtml(r) + '</div>';
                } else if (!l && r) {
                    leftHtml += '<div class="diff-line removed">&nbsp;</div>';
                    rightHtml += '<div class="diff-line added">' + escapeHtml(r) + '</div>';
                } else if (l && !r) {
                    leftHtml += '<div class="diff-line removed">' + escapeHtml(l) + '</div>';
                    rightHtml += '<div class="diff-line added">&nbsp;</div>';
                } else {
                    leftHtml += '<div class="diff-line removed">' + escapeHtml(l) + '</div>';
                    rightHtml += '<div class="diff-line added">' + escapeHtml(r) + '</div>';
                }
            }

            return { leftHtml, rightHtml };
        }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text;
            return div.innerHTML;
        }

        // ===== QR CODE =====

        function showQRCode(url) {
            document.getElementById('qr-url').textContent = url;
            document.getElementById('qr-modal').classList.add('visible');
            generateQRCode(url);
        }

        function closeQRModal(event) {
            if (event && event.target !== event.currentTarget) return;
            document.getElementById('qr-modal').classList.remove('visible');
        }

        // Render QR code using Google Charts API (simple fallback)
        function generateQRCode(text) {
            const canvas = document.getElementById('qr-canvas');
            const ctx = canvas.getContext('2d');
            const size = 200;

            // Use an image from Google Charts QR API
            const img = new Image();
            img.crossOrigin = 'anonymous';
            img.onload = function() {
                ctx.fillStyle = 'white';
                ctx.fillRect(0, 0, size, size);
                ctx.drawImage(img, 0, 0, size, size);
            };
            img.onerror = function() {
                // Fallback: draw placeholder with URL text
                ctx.fillStyle = 'white';
                ctx.fillRect(0, 0, size, size);
                ctx.fillStyle = '#333';
                ctx.font = '12px monospace';
                ctx.textAlign = 'center';
                ctx.fillText('QR Code', size/2, size/2 - 10);
                ctx.fillText('(scan URL below)', size/2, size/2 + 10);
            };
            img.src = 'https://chart.googleapis.com/chart?cht=qr&chs=' + size + 'x' + size + '&chl=' + encodeURIComponent(text) + '&choe=UTF-8';
        }

        // ===== WEBHOOK TESTER =====

        function showWebhookTester(url) {
            document.getElementById('webhook-url').value = url || '';
            document.getElementById('webhook-headers').value = '{"Content-Type": "application/json"}';
            document.getElementById('webhook-body').value = '';
            document.getElementById('webhook-response').classList.remove('visible');
            document.getElementById('webhook-modal').classList.add('visible');
        }

        function closeWebhookModal(event) {
            if (event && event.target !== event.currentTarget) return;
            document.getElementById('webhook-modal').classList.remove('visible');
        }

        function sendWebhookTest() {
            const method = document.getElementById('webhook-method').value;
            const url = document.getElementById('webhook-url').value;
            const headersStr = document.getElementById('webhook-headers').value;
            const body = document.getElementById('webhook-body').value;
            const btn = document.getElementById('btn-webhook-send');
            const responseDiv = document.getElementById('webhook-response');

            if (!url) {
                alert('URL is required');
                return;
            }

            let headers = {};
            if (headersStr.trim()) {
                try {
                    headers = JSON.parse(headersStr);
                } catch (e) {
                    alert('Invalid JSON in headers');
                    return;
                }
            }

            btn.disabled = true;
            btn.textContent = 'Sending...';

            fetch('/api/webhook-test', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({ url, method, headers, body })
            })
            .then(r => r.json())
            .then(data => {
                btn.disabled = false;
                btn.textContent = 'Send Request';
                responseDiv.classList.add('visible');

                const statusEl = document.getElementById('webhook-response-status');
                const timeEl = document.getElementById('webhook-response-time');
                const bodyEl = document.getElementById('webhook-response-body');

                if (data.error) {
                    statusEl.textContent = 'Error: ' + data.error;
                    statusEl.className = 'webhook-response-status error';
                    bodyEl.textContent = '';
                } else {
                    statusEl.textContent = data.status_text;
                    statusEl.className = 'webhook-response-status ' + (data.status < 400 ? 'success' : 'error');
                    bodyEl.textContent = formatResponseBody(data.body, data.content_type);
                }
                timeEl.textContent = formatDuration(data.duration_ms);
            })
            .catch(err => {
                btn.disabled = false;
                btn.textContent = 'Send Request';
                alert('Error: ' + err.message);
            });
        }

        function formatResponseBody(body, contentType) {
            if (!body) return '(empty response)';
            if (contentType && contentType.includes('application/json')) {
                try {
                    return JSON.stringify(JSON.parse(body), null, 2);
                } catch {
                    return body;
                }
            }
            return body;
        }

        // ===== WEBHOOK TEMPLATES =====
        const TEMPLATES_KEY = 'tunnel_webhook_templates';
        let webhookTemplates = [];

        function loadWebhookTemplates() {
            try {
                const saved = localStorage.getItem(TEMPLATES_KEY);
                if (saved) {
                    webhookTemplates = JSON.parse(saved);
                }
            } catch (e) {
                console.warn('Failed to load templates:', e);
            }
            updateTemplateSelect();
        }

        function saveWebhookTemplates() {
            try {
                localStorage.setItem(TEMPLATES_KEY, JSON.stringify(webhookTemplates));
            } catch (e) {
                console.warn('Failed to save templates:', e);
            }
        }

        function updateTemplateSelect() {
            const select = document.getElementById('webhook-template-select');
            if (!select) return;

            select.innerHTML = '<option value="">-- Templates --</option>';
            webhookTemplates.forEach((tpl, idx) => {
                const option = document.createElement('option');
                option.value = idx;
                option.textContent = tpl.name;
                select.appendChild(option);
            });
        }

        function loadTemplate(idx) {
            if (idx === '' || idx === null) return;
            const tpl = webhookTemplates[parseInt(idx)];
            if (!tpl) return;

            document.getElementById('webhook-method').value = tpl.method || 'POST';
            document.getElementById('webhook-url').value = tpl.url || '';
            document.getElementById('webhook-headers').value = tpl.headers || '';
            document.getElementById('webhook-body').value = tpl.body || '';
            showCopyToast('Template loaded: ' + tpl.name);
        }

        function saveAsTemplate() {
            const name = prompt('Enter template name:');
            if (!name) return;

            const tpl = {
                name: name,
                method: document.getElementById('webhook-method').value,
                url: document.getElementById('webhook-url').value,
                headers: document.getElementById('webhook-headers').value,
                body: document.getElementById('webhook-body').value
            };

            webhookTemplates.push(tpl);
            saveWebhookTemplates();
            updateTemplateSelect();
            showCopyToast('Template saved: ' + name);
        }

        function deleteTemplate() {
            const select = document.getElementById('webhook-template-select');
            const idx = select.value;
            if (idx === '' || idx === null) {
                showCopyToast('Select a template first');
                return;
            }

            const tpl = webhookTemplates[parseInt(idx)];
            if (!confirm('Delete template "' + tpl.name + '"?')) return;

            webhookTemplates.splice(parseInt(idx), 1);
            saveWebhookTemplates();
            updateTemplateSelect();
            showCopyToast('Template deleted');
        }

        // ===== TUNNEL MANAGEMENT =====

        function loadTunnels() {
            fetch('/api/tunnels')
                .then(r => r.json())
                .then(data => {
                    tunnelsList = data || [];
                    filterTunnels(filterQuery);
                    updateTunnelCount();
                })
                .catch(err => {
                    console.error('Failed to load tunnels:', err);
                    renderTunnelError();
                });
        }

        function updateTunnelCount() {
            setText('tunnel-count', tunnelsList.length);
            // Show filter if more than 5 tunnels
            const filterEl = $('tunnel-filter');
            if (filterEl) {
                if (tunnelsList.length > 5) {
                    filterEl.classList.add('visible');
                } else {
                    filterEl.classList.remove('visible');
                }
            }
        }

        function filterTunnels(query) {
            filterQuery = query.toLowerCase();
            if (!filterQuery) {
                filteredTunnels = tunnelsList;
            } else {
                filteredTunnels = tunnelsList.filter(t => {
                    const searchStr = (t.public_url || '') + (t.subdomain || '') + t.local_addr + t.type;
                    return searchStr.toLowerCase().includes(filterQuery);
                });
            }
            renderTunnels();
        }

        function renderTunnels() {
            const container = $('tunnel-content');
            if (!container) return;
            container.replaceChildren();

            if (tunnelsList.length === 0) {
                renderEmptyState(container);
                return;
            }

            if (filteredTunnels.length === 0) {
                const noMatch = document.createElement('div');
                noMatch.className = 'tunnel-empty';
                noMatch.textContent = 'No tunnels match your filter';
                container.appendChild(noMatch);
                return;
            }

            // Group by type
            const httpTunnels = filteredTunnels.filter(t => t.type === 'http');
            const tcpTunnels = filteredTunnels.filter(t => t.type === 'tcp');

            if (httpTunnels.length > 0) {
                container.appendChild(createTunnelGroup('http', httpTunnels));
            }
            if (tcpTunnels.length > 0) {
                container.appendChild(createTunnelGroup('tcp', tcpTunnels));
            }
        }

        function createTunnelGroup(type, tunnels) {
            const group = document.createElement('div');
            group.className = 'tunnel-group' + (collapsedGroups[type] ? ' collapsed' : '');
            group.dataset.type = type;

            const header = document.createElement('div');
            header.className = 'tunnel-group-header';
            header.onclick = () => toggleTunnelGroup(type);

            const chevron = document.createElement('span');
            chevron.className = 'chevron';
            chevron.textContent = '▼';
            header.appendChild(chevron);

            const label = document.createElement('span');
            label.textContent = type.toUpperCase();
            header.appendChild(label);

            const line = document.createElement('span');
            line.className = 'line';
            header.appendChild(line);

            const count = document.createElement('span');
            count.className = 'count';
            count.textContent = '(' + tunnels.length + ')';
            header.appendChild(count);

            group.appendChild(header);

            const body = document.createElement('div');
            body.className = 'tunnel-group-body';

            const tree = document.createElement('div');
            tree.className = 'tunnel-tree';

            tunnels.forEach((tunnel, index) => {
                const isLast = index === tunnels.length - 1;
                tree.appendChild(createTunnelItem(tunnel, isLast, type));
            });

            body.appendChild(tree);
            group.appendChild(body);

            return group;
        }

        function createTunnelItem(tunnel, isLast, type) {
            const item = document.createElement('div');
            item.className = 'tunnel-item';

            // Tree connector
            const connector = document.createElement('div');
            connector.className = 'tree-connector';
            const line = document.createElement('span');
            line.className = 'line';
            line.textContent = isLast ? '└─' : '├─';
            connector.appendChild(line);
            item.appendChild(connector);

            // Content
            const content = document.createElement('div');
            content.className = 'tunnel-content';

            // Main row
            const mainRow = document.createElement('div');
            mainRow.className = 'tunnel-main-row';

            const status = document.createElement('div');
            status.className = 'tunnel-status';
            status.title = 'Active';
            mainRow.appendChild(status);

            const url = document.createElement('span');
            url.className = 'tunnel-url';
            if (type === 'http') {
                url.textContent = tunnel.public_url || tunnel.subdomain + '.px.csam.dev';
            } else {
                url.textContent = tunnel.public_url || 'tcp://px.csam.dev:' + tunnel.remote_port;
            }
            url.title = url.textContent;
            mainRow.appendChild(url);

            // Actions
            const actions = document.createElement('div');
            actions.className = 'tunnel-actions';

            const copyBtn = document.createElement('button');
            copyBtn.className = 'tunnel-action-btn';
            copyBtn.textContent = '⎘';
            copyBtn.title = 'Copy URL';
            copyBtn.onclick = (e) => { e.stopPropagation(); copyTunnelUrl(tunnel, copyBtn); };
            actions.appendChild(copyBtn);

            const cmdBtn = document.createElement('button');
            cmdBtn.className = 'tunnel-action-btn';
            cmdBtn.textContent = type === 'http' ? '⧉' : '⌘';
            cmdBtn.title = type === 'http' ? 'Copy curl command' : 'Copy SSH command';
            cmdBtn.onclick = (e) => { e.stopPropagation(); copyTunnelCommand(tunnel, cmdBtn); };
            actions.appendChild(cmdBtn);

            const qrBtn = document.createElement('button');
            qrBtn.className = 'tunnel-action-btn';
            qrBtn.textContent = '⊞';
            qrBtn.title = 'Show QR code';
            qrBtn.onclick = (e) => { e.stopPropagation(); showQRCode(tunnel.public_url); };
            actions.appendChild(qrBtn);

            if (type === 'http') {
                const testBtn = document.createElement('button');
                testBtn.className = 'tunnel-action-btn';
                testBtn.textContent = '⚡';
                testBtn.title = 'Test webhook';
                testBtn.onclick = (e) => { e.stopPropagation(); showWebhookTester(tunnel.public_url); };
                actions.appendChild(testBtn);
            }

            const removeBtn = document.createElement('button');
            removeBtn.className = 'tunnel-action-btn remove';
            removeBtn.textContent = '✕';
            removeBtn.title = 'Remove tunnel';
            removeBtn.onclick = (e) => { e.stopPropagation(); removeTunnel(tunnel); };
            actions.appendChild(removeBtn);

            mainRow.appendChild(actions);
            content.appendChild(mainRow);

            // Local row
            const localRow = document.createElement('div');
            localRow.className = 'tunnel-local-row';

            const elbow = document.createElement('span');
            elbow.className = 'tree-elbow';
            elbow.textContent = '└─';
            localRow.appendChild(elbow);

            const arrow = document.createElement('span');
            arrow.className = 'tunnel-arrow';
            arrow.textContent = '→';
            localRow.appendChild(arrow);

            const local = document.createElement('span');
            local.className = 'tunnel-local';
            local.textContent = tunnel.local_addr;
            localRow.appendChild(local);

            content.appendChild(localRow);
            item.appendChild(content);

            return item;
        }

        function renderEmptyState(container) {
            const empty = document.createElement('div');
            empty.className = 'tunnel-empty';

            const icon = document.createElement('div');
            icon.className = 'tunnel-empty-icon';
            icon.textContent = '🚇';
            empty.appendChild(icon);

            const title = document.createElement('div');
            title.className = 'tunnel-empty-title';
            title.textContent = 'No tunnels configured';
            empty.appendChild(title);

            const hint = document.createElement('div');
            hint.className = 'tunnel-empty-hint';
            hint.textContent = 'Press N or click below to add one';
            empty.appendChild(hint);

            const btn = document.createElement('button');
            btn.className = 'btn-empty-add';
            btn.textContent = '+ Add Tunnel';
            btn.onclick = showAddTunnelModal;
            empty.appendChild(btn);

            container.appendChild(empty);
        }

        function renderTunnelError() {
            const container = $('tunnel-content');
            if (!container) return;
            container.replaceChildren();
            const err = document.createElement('div');
            err.className = 'tunnel-empty';
            err.textContent = 'Failed to load tunnels';
            container.appendChild(err);
        }

        function toggleTunnelPanel() {
            const panel = $('tunnel-panel');
            if (panel) panel.classList.toggle('collapsed');
        }

        function toggleTunnelGroup(type) {
            collapsedGroups[type] = !collapsedGroups[type];
            const group = document.querySelector('.tunnel-group[data-type="' + type + '"]');
            if (group) {
                group.classList.toggle('collapsed');
            }
        }

        function copyTunnelUrl(tunnel, btn) {
            const url = tunnel.public_url || (tunnel.type === 'http' ?
                'https://' + tunnel.subdomain + '.px.csam.dev' :
                'px.csam.dev:' + tunnel.remote_port);

            navigator.clipboard.writeText(url).then(() => {
                btn.classList.add('copy-success');
                btn.textContent = '✓';
                setTimeout(() => {
                    btn.classList.remove('copy-success');
                    btn.textContent = '⎘';
                }, 1500);
            });
        }

        function copyTunnelCommand(tunnel, btn) {
            let cmd;
            if (tunnel.type === 'http') {
                const url = tunnel.public_url || 'https://' + tunnel.subdomain + '.px.csam.dev';
                cmd = 'curl ' + url;
            } else {
                const parts = (tunnel.public_url || 'px.csam.dev:' + tunnel.remote_port).replace('tcp://', '').split(':');
                cmd = 'ssh -p ' + parts[1] + ' user@' + parts[0];
            }

            navigator.clipboard.writeText(cmd).then(() => {
                btn.classList.add('copy-success');
                btn.textContent = '✓';
                setTimeout(() => {
                    btn.classList.remove('copy-success');
                    btn.textContent = tunnel.type === 'http' ? '⧉' : '⌘';
                }, 1500);
            });
        }

        function showAddTunnelModal() {
            document.getElementById('add-tunnel-modal').classList.add('visible');
            document.getElementById('tunnel-subdomain').value = '';
            document.getElementById('tunnel-port').value = '0';
            document.getElementById('tunnel-local').value = '';
            document.getElementById('tunnel-persist').checked = true;
            document.querySelector('input[name="tunnel-type"][value="http"]').checked = true;
            updateFormFields();
            updatePreview();
            document.getElementById('tunnel-local').focus();
        }

        function hideAddTunnelModal() {
            document.getElementById('add-tunnel-modal').classList.remove('visible');
        }

        function updateFormFields() {
            const type = document.querySelector('input[name="tunnel-type"]:checked').value;
            document.getElementById('type-http').classList.toggle('selected', type === 'http');
            document.getElementById('type-tcp').classList.toggle('selected', type === 'tcp');
            document.getElementById('subdomain-group').style.display = type === 'http' ? 'block' : 'none';
            document.getElementById('port-group').style.display = type === 'tcp' ? 'block' : 'none';
        }

        function updatePreview() {
            const subdomain = document.getElementById('tunnel-subdomain').value.trim();
            const preview = document.getElementById('subdomain-preview');
            if (subdomain) {
                preview.textContent = '→ ' + subdomain + '.px.csam.dev';
            } else {
                preview.textContent = '→ random.px.csam.dev';
            }
        }

        function submitAddTunnel(event) {
            event.preventDefault();

            const type = document.querySelector('input[name="tunnel-type"]:checked').value;
            const subdomain = document.getElementById('tunnel-subdomain').value.trim();
            const remotePort = parseInt(document.getElementById('tunnel-port').value) || 0;
            const localAddr = document.getElementById('tunnel-local').value.trim();
            const persist = document.getElementById('tunnel-persist').checked;

            if (!localAddr) {
                alert('Local address is required');
                return;
            }

            const btn = document.getElementById('btn-add-submit');
            btn.disabled = true;
            btn.textContent = 'Adding...';

            const body = { type, local_addr: localAddr, persist };
            if (type === 'http') {
                body.subdomain = subdomain;
            } else {
                body.remote_port = remotePort;
            }

            fetch('/api/tunnels/add', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            })
            .then(r => r.json())
            .then(data => {
                btn.disabled = false;
                btn.textContent = 'Add Tunnel ⏎';

                if (data.error) {
                    alert('Error: ' + data.error);
                } else {
                    hideAddTunnelModal();
                    loadTunnels();
                }
            })
            .catch(err => {
                btn.disabled = false;
                btn.textContent = 'Add Tunnel ⏎';
                alert('Error: ' + err.message);
            });
        }

        function removeTunnel(tunnel) {
            const name = tunnel.public_url || tunnel.subdomain || 'Port ' + tunnel.remote_port;
            if (!confirm('Remove tunnel?\n\n' + name + ' → ' + tunnel.local_addr)) {
                return;
            }

            const body = { type: tunnel.type, persist: true };
            if (tunnel.type === 'http') {
                body.subdomain = tunnel.subdomain;
            } else {
                body.remote_port = tunnel.remote_port;
            }

            fetch('/api/tunnels/remove', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            })
            .then(r => r.json())
            .then(data => {
                if (data.error) {
                    alert('Error: ' + data.error);
                } else {
                    loadTunnels();
                }
            })
            .catch(err => alert('Error: ' + err.message));
        }

        // ===== REQUEST LIST =====

        function loadInitial() {
            fetch('/api/requests')
                .then(r => r.json())
                .then(data => {
                    requestsList = data || [];
                    requestsList.forEach(r => requestsMap[r.id] = r);
                    renderList();
                });
        }

        function initSSE() {
            const evtSource = new EventSource('/api/events');

            evtSource.addEventListener('connected', function() {
                sseConnected = true;
                updateConnectionStatus(true);
            });

            evtSource.addEventListener('request', function(e) {
                try {
                    const req = JSON.parse(e.data);
                    if (isPaused) {
                        pausedQueue.push(req);
                        updatePauseBadge(pausedQueue.length);
                    } else {
                        addRequest(req);
                    }
                } catch (err) {
                    console.error('Failed to parse SSE:', err);
                }
            });

            evtSource.onerror = function() {
                sseConnected = false;
                updateConnectionStatus(false);
            };
        }

        function updateConnectionStatus(connected) {
            const badge = $('traffic-badge');
            const dot = $('connection-status');
            const statusText = $('tunnel-status-text');

            if (connected) {
                if (!connectionStartTime) {
                    connectionStartTime = new Date();
                }
                lastHeartbeat = new Date();

                if (badge) {
                    badge.classList.remove('reconnecting');
                    const span = badge.querySelector('span');
                    if (span) span.textContent = 'Live';
                }
                if (dot) {
                    dot.classList.remove('disconnected');
                    dot.title = getConnectionTooltip();
                }
                if (statusText) {
                    statusText.textContent = '● Connected';
                    statusText.style.color = 'var(--success)';
                }
            } else {
                reconnectCount++;
                connectionStartTime = null;

                if (badge) {
                    badge.classList.add('reconnecting');
                    const span = badge.querySelector('span');
                    if (span) span.textContent = 'Reconnecting...';
                }
                if (dot) {
                    dot.classList.add('disconnected');
                    dot.title = 'Disconnected - Reconnect #' + reconnectCount;
                }
                if (statusText) {
                    statusText.textContent = '○ Reconnecting...';
                    statusText.style.color = 'var(--warning)';
                }
            }
        }

        function getConnectionTooltip() {
            let tooltip = 'Connected';
            if (lastHeartbeat) {
                const ago = Math.round((new Date() - lastHeartbeat) / 1000);
                tooltip += ' • Last heartbeat: ' + (ago < 2 ? 'just now' : ago + 's ago');
            }
            if (connectionStartTime) {
                const uptime = Math.round((new Date() - connectionStartTime) / 1000);
                tooltip += ' • Uptime: ' + formatUptime(uptime);
            }
            if (reconnectCount > 0) {
                tooltip += ' • Reconnects: ' + reconnectCount;
            }
            return tooltip;
        }

        function formatUptime(seconds) {
            if (seconds < 60) return seconds + 's';
            if (seconds < 3600) return Math.floor(seconds / 60) + 'm ' + (seconds % 60) + 's';
            const h = Math.floor(seconds / 3600);
            const m = Math.floor((seconds % 3600) / 60);
            return h + 'h ' + m + 'm';
        }

        // ===== NOTIFICATIONS =====

        async function toggleNotifications() {
            const btn = document.getElementById('btn-notify');

            if (!notificationsEnabled) {
                if ('Notification' in window) {
                    const perm = await Notification.requestPermission();
                    if (perm === 'granted') {
                        notificationsEnabled = true;
                        btn.classList.add('active');
                        btn.title = 'Disable desktop notifications';
                        showCopyToast('Notifications enabled for errors');
                    } else {
                        showCopyToast('Notification permission denied');
                    }
                } else {
                    showCopyToast('Notifications not supported');
                }
            } else {
                notificationsEnabled = false;
                btn.classList.remove('active');
                btn.title = 'Enable desktop notifications for errors';
                showCopyToast('Notifications disabled');
            }
        }

        function notifyError(req) {
            if (notificationsEnabled && req.status >= 500) {
                new Notification('Request Error', {
                    body: req.method + ' ' + req.path + ' → ' + req.status,
                    tag: req.id,
                    icon: 'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="90">⚠️</text></svg>'
                });
            }
        }

        function exportHAR() {
            const a = document.createElement('a');
            a.href = '/api/export/har';
            a.download = 'tunnel-requests.har';
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            showCopyToast('HAR file downloaded');
        }

        // ===== PAUSE/RESUME =====

        function togglePause() {
            isPaused = !isPaused;
            updatePauseButton();

            if (!isPaused && pausedQueue.length > 0) {
                pausedQueue.forEach(r => {
                    requestsMap[r.id] = r;
                    requestsList.unshift(r);
                });
                pausedQueue = [];
                filterRequests();
                showCopyToast('Resumed - ' + requestsList.length + ' requests');
            } else if (isPaused) {
                showCopyToast('Paused - new requests will queue');
            }
        }

        function updatePauseButton() {
            const btn = document.getElementById('btn-pause');
            const icon = document.getElementById('pause-icon');
            const badge = document.getElementById('pause-badge');

            if (isPaused) {
                btn.classList.add('paused');
                icon.textContent = '▶';
                btn.title = 'Resume live updates (Space)';
            } else {
                btn.classList.remove('paused');
                icon.textContent = '⏸';
                btn.title = 'Pause live updates (Space)';
                badge.style.display = 'none';
            }
        }

        function updatePauseBadge(count) {
            const badge = document.getElementById('pause-badge');
            if (isPaused && count > 0) {
                badge.textContent = count > 99 ? '99+' : count;
                badge.style.display = 'inline';
            } else {
                badge.style.display = 'none';
            }
        }

        // ===== THEME TOGGLE =====

        function toggleTheme() {
            theme = theme === 'dark' ? 'light' : 'dark';
            applyTheme();
            localStorage.setItem('tunnel_theme', theme);
            showCopyToast(theme === 'dark' ? 'Dark theme' : 'Light theme');
        }

        function applyTheme() {
            document.documentElement.classList.toggle('light-theme', theme === 'light');
            const icon = $('theme-icon');
            if (icon) icon.textContent = theme === 'dark' ? '🌙' : '☀️';
        }

        // ===== KEYBOARD SHORTCUTS =====

        function showShortcutsModal() {
            const modal = $('shortcuts-modal');
            if (modal) modal.classList.add('visible');
        }

        function closeShortcutsModal(event) {
            if (event && event.target !== event.currentTarget) return;
            const modal = $('shortcuts-modal');
            if (modal) modal.classList.remove('visible');
        }

        function navigateRequest(direction) {
            if (filteredRequestsList.length === 0) return;

            let currentIndex = -1;
            if (selectedId) {
                currentIndex = filteredRequestsList.findIndex(r => r.id === selectedId);
            }

            let newIndex = currentIndex + direction;
            if (newIndex < 0) newIndex = 0;
            if (newIndex >= filteredRequestsList.length) newIndex = filteredRequestsList.length - 1;

            if (newIndex !== currentIndex) {
                selectRequest(filteredRequestsList[newIndex].id);
            }
        }

        // ========== Request Pinning ==========
        function togglePin(id) {
            if (pinnedRequests.has(id)) {
                pinnedRequests.delete(id);
                showCopyToast('Request unpinned');
            } else {
                pinnedRequests.add(id);
                showCopyToast('Request pinned');
            }
            updatePinnedCount();
            renderRequests();
            saveSession();
        }

        function updatePinnedCount() {
            const count = pinnedRequests.size;
            const countEl = document.getElementById('pinned-count');
            const numEl = document.getElementById('pinned-num');
            const clearBtn = document.getElementById('btn-clear-session');

            if (count > 0) {
                countEl.style.display = 'inline';
                numEl.textContent = count;
            } else {
                countEl.style.display = 'none';
            }

            // Show clear button if there's session data
            const hasSessionData = count > 0 || Object.keys(annotations).length > 0;
            clearBtn.style.display = hasSessionData ? 'inline' : 'none';
        }

        // ========== Request Annotations ==========
        function setAnnotation(id, note, tags) {
            if (!note && (!tags || tags.length === 0)) {
                delete annotations[id];
            } else {
                annotations[id] = { note: note || '', tags: tags || [] };
            }
            renderRequests();
            // Update display if this is the selected request
            if (id === selectedId) {
                renderAnnotationDisplay(id);
            }
            saveSession();
        }

        function getAllTags() {
            const tagSet = new Set();
            Object.values(annotations).forEach(a => {
                if (a.tags) a.tags.forEach(t => tagSet.add(t));
            });
            return Array.from(tagSet).sort();
        }

        // Helper to close any existing dynamic modals before opening a new one
        function closeDynamicModals() {
            document.querySelectorAll('.modal-overlay[data-dynamic]').forEach(m => m.remove());
        }

        function showAnnotationModal(requestId) {
            if (!requestId) {
                showCopyToast('Select a request first');
                return;
            }
            closeDynamicModals();
            const existing = annotations[requestId] || { note: '', tags: [] };
            const modal = document.createElement('div');
            modal.className = 'modal-overlay visible';
            modal.setAttribute('data-dynamic', 'true');
            modal.innerHTML = `
                <div class="modal-content" style="width: 400px;">
                    <div class="modal-header">
                        <h3>Annotate Request</h3>
                        <button class="modal-close" onclick="this.closest('.modal-overlay').remove()">&times;</button>
                    </div>
                    <div class="modal-body">
                        <div style="margin-bottom: 12px;">
                            <label style="display: block; margin-bottom: 4px; font-weight: 500;">Note</label>
                            <textarea class="annotation-note-textarea" style="width: 100%; height: 80px; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px; resize: vertical;">${existing.note}</textarea>
                        </div>
                        <div>
                            <label style="display: block; margin-bottom: 4px; font-weight: 500;">Tags (comma-separated)</label>
                            <input type="text" class="annotation-tags-input" value="${existing.tags.join(', ')}" style="width: 100%; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px;">
                            <div class="tag-suggestions-container" style="margin-top: 8px; display: flex; flex-wrap: wrap; gap: 4px;"></div>
                        </div>
                    </div>
                    <div class="modal-footer" style="display: flex; gap: 8px; justify-content: flex-end; margin-top: 16px;">
                        <button onclick="this.closest('.modal-overlay').remove()" style="padding: 8px 16px; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); cursor: pointer;">Cancel</button>
                        <button class="save-annotation-btn" style="padding: 8px 16px; background: var(--accent); border: none; border-radius: 4px; color: white; cursor: pointer;">Save</button>
                    </div>
                </div>
            `;
            document.body.appendChild(modal);

            // Show existing tag suggestions
            const allTags = getAllTags();
            const suggestionsDiv = modal.querySelector('.tag-suggestions-container');
            allTags.forEach(tag => {
                const btn = document.createElement('button');
                btn.textContent = tag;
                btn.style.cssText = 'padding: 2px 8px; background: var(--bg-secondary); border: 1px solid var(--border); border-radius: 12px; color: var(--text-secondary); cursor: pointer; font-size: 11px;';
                btn.onclick = () => {
                    const input = modal.querySelector('.annotation-tags-input');
                    const currentTags = input.value.split(',').map(t => t.trim()).filter(t => t);
                    if (!currentTags.includes(tag)) {
                        currentTags.push(tag);
                        input.value = currentTags.join(', ');
                    }
                };
                suggestionsDiv.appendChild(btn);
            });

            // Save handler
            modal.querySelector('.save-annotation-btn').onclick = () => {
                const note = modal.querySelector('.annotation-note-textarea').value.trim();
                const tagsStr = modal.querySelector('.annotation-tags-input').value;
                const tags = tagsStr.split(',').map(t => t.trim()).filter(t => t);
                setAnnotation(requestId, note, tags);
                modal.remove();
                showCopyToast('Annotation saved');
            };

            // Close on overlay click
            modal.onclick = (e) => {
                if (e.target === modal) modal.remove();
            };
        }

        function renderAnnotationDisplay(requestId) {
            const display = document.getElementById('annotation-display');
            const ann = annotations[requestId];

            if (!ann || (!ann.note && (!ann.tags || ann.tags.length === 0))) {
                display.style.display = 'none';
                return;
            }

            display.style.display = 'block';
            let html = '';

            if (ann.note) {
                html += '<div class="annotation-note">' + escapeHtml(ann.note) + '</div>';
            }

            if (ann.tags && ann.tags.length > 0) {
                html += '<div class="annotation-tags">';
                ann.tags.forEach(tag => {
                    html += '<span class="annotation-tag">' + escapeHtml(tag) + '</span>';
                });
                html += '</div>';
            }

            display.innerHTML = html;
        }

        // ========== Session Persistence ==========
        const SESSION_KEY = 'tunnel_inspector_session';
        const SESSION_MAX_AGE = 60 * 60 * 1000; // 1 hour

        function saveSession() {
            try {
                // Only save minimal request data (no bodies) to avoid quota issues
                const minimalRequests = requestsList.slice(0, 20).map(r => ({
                    id: r.id,
                    method: r.method,
                    path: r.path,
                    host: r.host,
                    status: r.status,
                    duration: r.duration,
                    timestamp: r.timestamp,
                    tunnel_id: r.tunnel_id
                }));
                const session = {
                    requests: minimalRequests,
                    pinnedRequests: Array.from(pinnedRequests),
                    annotations: annotations,
                    timestamp: Date.now()
                };
                localStorage.setItem(SESSION_KEY, JSON.stringify(session));
            } catch (e) {
                // If quota exceeded, clear old data and try again
                if (e.name === 'QuotaExceededError') {
                    localStorage.removeItem(SESSION_KEY);
                    console.warn('Session storage quota exceeded, cleared old data');
                } else {
                    console.warn('Failed to save session:', e);
                }
            }
        }

        function loadSession() {
            try {
                const saved = localStorage.getItem(SESSION_KEY);
                if (!saved) return false;

                const session = JSON.parse(saved);
                const age = Date.now() - session.timestamp;

                if (age > SESSION_MAX_AGE) {
                    localStorage.removeItem(SESSION_KEY);
                    return false;
                }

                // Restore requests
                if (session.requests && session.requests.length > 0) {
                    session.requests.forEach(r => {
                        requestsMap[r.id] = r;
                        requestsList.push(r);
                    });
                }

                // Restore pinned requests
                if (session.pinnedRequests) {
                    session.pinnedRequests.forEach(id => pinnedRequests.add(id));
                }

                // Restore annotations
                if (session.annotations) {
                    Object.assign(annotations, session.annotations);
                }

                filterRequests();
                updatePinnedCount();
                const ageMinutes = Math.floor(age / 60000);
                showCopyToast('Session restored (' + ageMinutes + 'm ago)');
                return true;
            } catch (e) {
                console.warn('Failed to load session:', e);
                return false;
            }
        }

        function clearSession() {
            localStorage.removeItem(SESSION_KEY);
            pinnedRequests.clear();
            Object.keys(annotations).forEach(k => delete annotations[k]);
            updatePinnedCount();
            renderRequests();
            // Clear annotation display if visible
            const display = document.getElementById('annotation-display');
            if (display) display.style.display = 'none';
            showCopyToast('Session cleared');
        }

        // Auto-save periodically
        setInterval(saveSession, 30000);

        // Save on page unload
        window.addEventListener('beforeunload', saveSession);

        function addRequest(r) {
            // Handle pause mode
            if (isPaused) {
                pausedQueue.push(r);
                updatePauseButton();
                return;
            }

            requestsMap[r.id] = r;
            requestsList.unshift(r);

            // Limit to 100 requests, but skip pinned ones
            while (requestsList.length > 100) {
                // Find first non-pinned request from the end to remove
                let removeIndex = -1;
                for (let i = requestsList.length - 1; i >= 0; i--) {
                    if (!pinnedRequests.has(requestsList[i].id)) {
                        removeIndex = i;
                        break;
                    }
                }
                if (removeIndex === -1) break; // All are pinned, can't remove any
                const removed = requestsList.splice(removeIndex, 1)[0];
                delete requestsMap[removed.id];
            }

            // Re-apply filters
            filterRequests();
            // Notify on errors
            notifyError(r);
            // Save session periodically (debounced via interval)
        }

        function renderList() {
            // Use filtered list
            filterRequests();
        }

        // Alias for renderFilteredRequests - used after diff selection and other operations
        function renderRequests() {
            renderFilteredRequests();
        }

        function createRequestItem(r) {
            const statusClass = r.status >= 500 ? 's-5xx' : (r.status >= 400 ? 's-4xx' : 's-2xx');
            const isPinned = pinnedRequests.has(r.id);
            const hasAnnotation = annotations[r.id];

            const item = document.createElement('div');
            item.className = 'req-item' + (r.id === selectedId ? ' active' : '') + (isPinned ? ' pinned' : '');
            item.onclick = (e) => {
                if (e.target.type !== 'checkbox' && !e.target.classList.contains('req-pin-btn')) {
                    selectRequest(r.id);
                }
            };

            const checkbox = document.createElement('input');
            checkbox.type = 'checkbox';
            checkbox.className = 'diff-checkbox';
            checkbox.checked = selectedForDiff.includes(r.id);
            checkbox.onclick = (e) => {
                e.stopPropagation();
                toggleDiffSelection(r.id, e.target.checked);
            };
            item.appendChild(checkbox);

            const pinBtn = document.createElement('button');
            pinBtn.className = 'req-pin-btn' + (isPinned ? ' pinned' : '');
            pinBtn.textContent = '📌';
            pinBtn.title = isPinned ? 'Unpin request' : 'Pin request';
            pinBtn.onclick = (e) => {
                e.stopPropagation();
                togglePin(r.id);
            };
            item.appendChild(pinBtn);

            const dot = document.createElement('div');
            dot.className = 'req-status-dot ' + statusClass;
            item.appendChild(dot);

            const info = document.createElement('div');
            info.className = 'req-info';

            const main = document.createElement('div');
            main.className = 'req-main';

            const method = document.createElement('span');
            method.className = 'req-method ' + statusClass;
            method.textContent = r.method;
            main.appendChild(method);

            const path = document.createElement('span');
            path.className = 'req-path';
            path.textContent = r.path;
            if (hasAnnotation) {
                const noteIcon = document.createElement('span');
                noteIcon.className = 'req-annotation-icon';
                noteIcon.textContent = '📝';
                noteIcon.title = hasAnnotation.note || 'Has annotation';
                path.appendChild(noteIcon);
            }
            main.appendChild(path);

            info.appendChild(main);

            const meta = document.createElement('div');
            meta.className = 'req-meta';

            const status = document.createElement('span');
            status.textContent = r.status;
            meta.appendChild(status);

            const duration = document.createElement('span');
            duration.textContent = formatDuration(r.duration_ms);
            meta.appendChild(duration);

            const time = document.createElement('span');
            time.textContent = new Date(r.timestamp).toLocaleTimeString();
            meta.appendChild(time);

            info.appendChild(meta);
            item.appendChild(info);

            return item;
        }

        function selectRequest(id) {
            selectedId = id;
            const r = requestsMap[id];
            if (!r) return;

            document.getElementById('empty-state').style.display = 'none';
            document.getElementById('detail-view').classList.add('visible');

            const statusClass = r.status >= 500 ? 's-5xx' : (r.status >= 400 ? 's-4xx' : 's-2xx');

            const methodEl = document.getElementById('d-method');
            methodEl.textContent = r.method;
            methodEl.className = 'detail-method ' + statusClass;

            document.getElementById('d-path').textContent = r.path;

            const statusEl = document.getElementById('d-status');
            statusEl.textContent = r.status;
            statusEl.className = 'detail-status ' + statusClass;

            document.getElementById('d-time').textContent = new Date(r.timestamp).toLocaleString();
            document.getElementById('d-duration').textContent = formatDuration(r.duration_ms);

            // Show annotation if exists
            renderAnnotationDisplay(id);

            renderHeaders('req', r.req_header);
            renderHeaders('res', r.res_header);
            renderBody('req', r.req_body, r.req_header ? r.req_header['Content-Type'] : '');
            renderBody('res', r.res_body, r.content_type || (r.res_header ? r.res_header['Content-Type'] : ''));

            document.getElementById('replay-result').classList.remove('visible');
            renderList();
        }

        function replayRequest() {
            if (!selectedId) return;

            const btn = document.getElementById('btn-replay');
            const resultDiv = document.getElementById('replay-result');
            btn.disabled = true;
            btn.textContent = '⏳ Replaying...';
            resultDiv.classList.remove('visible');

            fetch('/api/replay/' + selectedId, { method: 'POST' })
                .then(r => r.json())
                .then(data => {
                    btn.disabled = false;
                    btn.textContent = '▶ Replay';
                    resultDiv.classList.add('visible');

                    if (data.error) {
                        resultDiv.className = 'replay-result visible error';
                        resultDiv.textContent = 'Error: ' + data.error;
                    } else {
                        resultDiv.className = 'replay-result visible success';
                        resultDiv.textContent = '✓ Replay completed: ' + data.status + ' (' + data.duration + ')';
                    }
                })
                .catch(err => {
                    btn.disabled = false;
                    btn.textContent = '▶ Replay';
                    resultDiv.classList.add('visible');
                    resultDiv.className = 'replay-result visible error';
                    resultDiv.textContent = 'Error: ' + err.message;
                });
        }

        // ========== MOCK MANAGEMENT ==========
        let mockRules = [];

        function loadMocks() {
            fetch('/api/mocks')
                .then(r => r.json())
                .then(data => {
                    mockRules = data || [];
                })
                .catch(err => console.error('Failed to load mocks:', err));
        }

        function createMockFromRequest() {
            if (!selectedId) {
                showCopyToast('Select a request first');
                return;
            }
            const r = requestsMap[selectedId];
            if (!r) {
                showCopyToast('Request not found');
                return;
            }

            showMockModal({
                pattern: r.path,
                method: r.method,
                status_code: r.status,
                headers: { 'Content-Type': r.content_type || 'application/json' },
                body: r.res_body || '',
                delay_ms: 0
            });
        }

        function showMockModal(defaults = {}) {
            closeDynamicModals();
            const modal = document.createElement('div');
            modal.className = 'modal-overlay visible';
            modal.setAttribute('data-dynamic', 'true');
            modal.innerHTML = `
                <div class="modal-content" style="width: 500px;">
                    <div class="modal-header">
                        <h3>Create Mock Rule</h3>
                        <button class="modal-close" onclick="this.closest('.modal-overlay').remove()">&times;</button>
                    </div>
                    <div class="modal-body">
                        <div style="margin-bottom: 12px;">
                            <label style="display: block; margin-bottom: 4px; font-weight: 500;">Pattern (glob)</label>
                            <input type="text" class="mock-pattern-input" value="${defaults.pattern || '/api/*'}" style="width: 100%; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px;">
                            <small style="color: var(--text-muted);">Use * for any segment, ** for any path, :param for path params</small>
                        </div>
                        <div style="display: flex; gap: 12px; margin-bottom: 12px;">
                            <div style="flex: 1;">
                                <label style="display: block; margin-bottom: 4px; font-weight: 500;">Method</label>
                                <select class="mock-method-select" style="width: 100%; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px;">
                                    <option value="*" ${defaults.method === '*' ? 'selected' : ''}>Any</option>
                                    <option value="GET" ${defaults.method === 'GET' ? 'selected' : ''}>GET</option>
                                    <option value="POST" ${defaults.method === 'POST' ? 'selected' : ''}>POST</option>
                                    <option value="PUT" ${defaults.method === 'PUT' ? 'selected' : ''}>PUT</option>
                                    <option value="DELETE" ${defaults.method === 'DELETE' ? 'selected' : ''}>DELETE</option>
                                    <option value="PATCH" ${defaults.method === 'PATCH' ? 'selected' : ''}>PATCH</option>
                                </select>
                            </div>
                            <div style="flex: 1;">
                                <label style="display: block; margin-bottom: 4px; font-weight: 500;">Status Code</label>
                                <input type="number" class="mock-status-input" value="${defaults.status_code || 200}" style="width: 100%; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px;">
                            </div>
                            <div style="flex: 1;">
                                <label style="display: block; margin-bottom: 4px; font-weight: 500;">Delay (ms)</label>
                                <input type="number" class="mock-delay-input" value="${defaults.delay_ms || 0}" style="width: 100%; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px;">
                            </div>
                        </div>
                        <div style="margin-bottom: 12px;">
                            <label style="display: block; margin-bottom: 4px; font-weight: 500;">Response Body</label>
                            <textarea class="mock-body-textarea" style="width: 100%; height: 120px; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px; font-family: monospace; resize: vertical;">${defaults.body || '{"success": true}'}</textarea>
                        </div>
                    </div>
                    <div class="modal-footer" style="display: flex; gap: 8px; justify-content: flex-end; margin-top: 16px;">
                        <button onclick="this.closest('.modal-overlay').remove()" style="padding: 8px 16px; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); cursor: pointer;">Cancel</button>
                        <button class="save-mock-btn" style="padding: 8px 16px; background: var(--primary); border: none; border-radius: 4px; color: white; cursor: pointer;">Create Mock</button>
                    </div>
                </div>
            `;
            document.body.appendChild(modal);

            modal.querySelector('.save-mock-btn').onclick = () => {
                const rule = {
                    pattern: modal.querySelector('.mock-pattern-input').value,
                    method: modal.querySelector('.mock-method-select').value,
                    status_code: parseInt(modal.querySelector('.mock-status-input').value) || 200,
                    delay_ms: parseInt(modal.querySelector('.mock-delay-input').value) || 0,
                    body: modal.querySelector('.mock-body-textarea').value,
                    headers: { 'Content-Type': 'application/json' }
                };

                fetch('/api/mocks', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(rule)
                })
                .then(r => r.json())
                .then(data => {
                    if (data.error) {
                        showCopyToast('Error: ' + data.error);
                    } else {
                        mockRules.push(data);
                        showCopyToast('Mock created: ' + data.pattern);
                        modal.remove();
                    }
                })
                .catch(err => showCopyToast('Error: ' + err.message));
            };

            modal.onclick = (e) => {
                if (e.target === modal) modal.remove();
            };
        }

        // ========== SHARE REQUEST ==========
        function shareRequest() {
            if (!selectedId) {
                showCopyToast('Select a request first');
                return;
            }

            const btn = document.getElementById('btn-share');
            btn.disabled = true;
            btn.textContent = '...';

            fetch('/api/share', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ request_id: selectedId })
            })
            .then(r => r.json())
            .then(data => {
                btn.disabled = false;
                btn.textContent = '🔗';

                if (data.error) {
                    showCopyToast('Error: ' + data.error);
                    return;
                }

                const shareUrl = window.location.origin + '/api/shared/' + data.token;
                showShareModal(shareUrl, data.expires_at);
            })
            .catch(err => {
                btn.disabled = false;
                btn.textContent = '🔗';
                showCopyToast('Error: ' + err.message);
            });
        }

        function showShareModal(url, expiresAt) {
            closeDynamicModals();
            const expiryDate = new Date(expiresAt);
            const modal = document.createElement('div');
            modal.className = 'modal-overlay visible';
            modal.setAttribute('data-dynamic', 'true');
            modal.innerHTML = `
                <div class="modal-content" style="width: 450px;">
                    <div class="modal-header">
                        <h3>Share Request</h3>
                        <button class="modal-close" onclick="this.closest('.modal-overlay').remove()">&times;</button>
                    </div>
                    <div class="modal-body">
                        <p style="margin-bottom: 12px; color: var(--text-secondary);">Anyone with this link can view this request. Link expires in 24 hours.</p>
                        <div style="display: flex; gap: 8px;">
                            <input type="text" class="share-url-input" value="${url}" readonly style="flex: 1; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 4px; color: var(--text-primary); padding: 8px; font-size: 12px;">
                            <button class="copy-share-btn" style="padding: 8px 16px; background: var(--primary); border: none; border-radius: 4px; color: white; cursor: pointer;">Copy</button>
                        </div>
                        <p style="margin-top: 12px; font-size: 11px; color: var(--text-muted);">Expires: ${expiryDate.toLocaleString()}</p>
                    </div>
                </div>
            `;
            document.body.appendChild(modal);

            modal.querySelector('.copy-share-btn').onclick = () => {
                const input = modal.querySelector('.share-url-input');
                input.select();
                navigator.clipboard.writeText(input.value);
                showCopyToast('Share URL copied!');
            };

            modal.onclick = (e) => {
                if (e.target === modal) modal.remove();
            };
        }

        function renderHeaders(type, headers) {
            const container = document.getElementById(type + '-headers-list');
            container.replaceChildren();

            const entries = Object.entries(headers || {});
            if (entries.length === 0) {
                const empty = document.createElement('div');
                empty.style.cssText = 'grid-column: span 2; color: var(--text-muted); font-style: italic;';
                empty.textContent = 'No headers';
                container.appendChild(empty);
                return;
            }

            entries.forEach(([key, val]) => {
                const keyDiv = document.createElement('div');
                keyDiv.className = 'kv-key';
                keyDiv.textContent = key + ':';
                container.appendChild(keyDiv);

                const valDiv = document.createElement('div');
                valDiv.className = 'kv-val';
                valDiv.textContent = val.join(', ');
                container.appendChild(valDiv);
            });
        }

        function renderBody(type, body, contentType) {
            const el = document.getElementById(type + '-body-content');
            if (!body) {
                el.innerHTML = '<span style="color: var(--text-muted)">No content</span>';
                return;
            }

            const parts = body.split('\r\n\r\n');
            let content = parts.length > 1 ? parts.slice(1).join('\r\n\r\n') : body;

            // Apply syntax highlighting
            el.innerHTML = formatBody(content || body, contentType || '');
        }

        function switchTab(section, tabName) {
            const headers = document.getElementById(section + '-headers');
            const body = document.getElementById(section + '-body');
            const tabs = event.target.parentNode;

            Array.from(tabs.children).forEach(t => t.classList.remove('active'));
            event.target.classList.add('active');

            headers.classList.toggle('active', tabName === 'headers');
            body.classList.toggle('active', tabName === 'body');
        }

        // ===== KEYBOARD SHORTCUTS =====

        document.addEventListener('keydown', function(e) {
            // Don't trigger shortcuts when typing in inputs
            if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') {
                if (e.key === 'Escape') {
                    e.target.blur();
                    hideAddTunnelModal();
                    closeShortcutsModal();
                }
                return;
            }

            // ? - Show shortcuts
            if (e.key === '?') {
                e.preventDefault();
                showShortcutsModal();
                return;
            }

            // N - New tunnel
            if (e.key === 'n' || e.key === 'N') {
                e.preventDefault();
                showAddTunnelModal();
                return;
            }

            // Ctrl+K or Cmd+K - Focus filter
            if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
                e.preventDefault();
                const filter = document.getElementById('tunnel-filter-input');
                if (tunnelsList.length > 5) {
                    filter.focus();
                }
                return;
            }

            // J - Next request
            if (e.key === 'j') {
                e.preventDefault();
                navigateRequest(1);
                return;
            }

            // K - Previous request
            if (e.key === 'k') {
                e.preventDefault();
                navigateRequest(-1);
                return;
            }

            // R - Replay request
            if (e.key === 'r' && selectedId) {
                e.preventDefault();
                replayRequest();
                return;
            }

            // C - Copy as cURL
            if (e.key === 'c' && selectedId) {
                e.preventDefault();
                copyAsCurl();
                return;
            }

            // Space - Pause/resume
            if (e.key === ' ') {
                e.preventDefault();
                togglePause();
                return;
            }

            // T - Toggle theme
            if (e.key === 't') {
                e.preventDefault();
                toggleTheme();
                return;
            }

            // P - Pin/unpin selected request
            if (e.key === 'p') {
                if (selectedId) {
                    e.preventDefault();
                    togglePin(selectedId);
                }
                return;
            }

            // A - Annotate selected request
            if (e.key === 'a') {
                if (selectedId) {
                    e.preventDefault();
                    showAnnotationModal(selectedId);
                }
                return;
            }

            // Escape - Close modals
            if (e.key === 'Escape') {
                hideAddTunnelModal();
                closeShortcutsModal();
                closeDiffModal();
                closeQRModal();
                closeWebhookModal();
            }
        });

        // Initialize
        loadTunnels();
        loadSession(); // Restore previous session if available
        loadWebhookTemplates(); // Load saved webhook templates
        loadMocks(); // Load mock rules
        loadInitial();
        initSSE();
        fetchMetrics();
        applyTheme();

        // Update connection tooltip every 5 seconds
        setInterval(function() {
            if (sseConnected) {
                const dot = document.getElementById('connection-status');
                if (dot) dot.title = getConnectionTooltip();
            }
        }, 5000);
