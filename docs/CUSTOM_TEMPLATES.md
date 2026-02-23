# Custom Dashboard Templates

The tunnel inspector dashboard supports custom templates, allowing you to create your own dashboard UI while leveraging the built-in JavaScript functionality.

## Quick Start

```bash
# Use a custom template
tunnel http 3000 --inspect --template-path ./my-template.html

# Or in config file (~/tunnel.yaml)
template_path: "./my-template.html"
```

## Template Structure

A custom template is a single HTML file that includes:
1. Your custom CSS styles
2. Your HTML structure
3. The injected `base.js` (automatically added)

### Minimal Template Example

```html
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>My Custom Inspector</title>
    <style>
        /* Your CSS here */
        body { font-family: system-ui, sans-serif; margin: 0; padding: 20px; }
        .request-item { padding: 10px; border-bottom: 1px solid #eee; cursor: pointer; }
        .request-item:hover { background: #f5f5f5; }
        .request-item.selected { background: #e3f2fd; }
    </style>
</head>
<body>
    <!-- Request List -->
    <div id="request-list"></div>

    <!-- Request Details (optional) -->
    <div id="request-detail" style="display: none;">
        <h3>Request Details</h3>
        <div id="d-method"></div>
        <div id="d-path"></div>
        <div id="d-status"></div>
        <div id="d-time"></div>
        <div id="d-duration"></div>
    </div>

    {{BASE_JS}}
</body>
</html>
```

The `{{BASE_JS}}` placeholder is replaced with the shared JavaScript that handles:
- SSE connection for real-time updates
- Request data management
- API calls (replay, tunnels, metrics)
- Keyboard shortcuts
- Session persistence

## Available DOM Elements

The base JavaScript looks for these element IDs. All are **optional** - the JS is defensive and won't crash if elements are missing.

### Core Elements

| Element ID | Purpose |
|------------|---------|
| `request-list` | Container for request list items |
| `request-detail` | Container for selected request details |
| `empty-state` | Shown when no requests exist |

### Request Detail Elements

| Element ID | Content |
|------------|---------|
| `d-method` | HTTP method (GET, POST, etc.) |
| `d-path` | Request path |
| `d-status` | Response status code |
| `d-time` | Timestamp |
| `d-duration` | Request duration (auto-formatted) |
| `d-url` | Full URL |

### Header/Body Display

| Element ID | Content |
|------------|---------|
| `req-headers` | Request headers container |
| `res-headers` | Response headers container |
| `req-body` | Request body container |
| `res-body` | Response body container |

### Metrics Elements (optional)

| Element ID | Content |
|------------|---------|
| `metric-total` | Total request count |
| `metric-success` | 2xx response count |
| `metric-errors` | 4xx/5xx response count |
| `metric-avg-latency` | Average latency |
| `latency-chart` | Canvas for latency chart |
| `rate-chart` | Canvas for request rate chart |

### Controls (optional)

| Element ID | Purpose |
|------------|---------|
| `btn-clear` | Clear all requests button |
| `btn-replay` | Replay selected request |
| `search-input` | Filter requests by search term |
| `method-filter` | Filter by HTTP method |
| `status-filter` | Filter by status code range |
| `tunnel-filter` | Filter by tunnel |

### Tunnel Management (optional)

| Element ID | Purpose |
|------------|---------|
| `tunnels-list` | Container for tunnel cards |
| `add-tunnel-form` | Form for adding new tunnels |

## JavaScript API

The base.js exposes these functions you can call from your template:

### Request Management

```javascript
// Get all requests
requestsList  // Array of request objects

// Get selected request
selectedId    // Currently selected request ID
requestsMap.get(selectedId)  // Get request by ID

// Select a request programmatically
selectRequest(id)

// Clear all requests
clearRequests()
```

### Filtering

```javascript
// Apply filters programmatically
currentFilters = {
    search: 'api',
    method: 'POST',
    status: '2xx',
    tunnel: 'tunnel-id'
}
renderList()  // Re-render with filters
```

### Replay

```javascript
// Replay the selected request
replayRequest()
```

### Utilities

```javascript
// Format bytes for display
formatBytes(1024)  // "1.0 KB"

// Format duration with appropriate units
formatDuration(0.5)     // "500µs"
formatDuration(150)     // "150ms"
formatDuration(2500)    // "2.50s"

// Escape HTML
escapeHtml('<script>')  // "&lt;script&gt;"

// Format headers object
formatHeaders({ 'Content-Type': ['application/json'] })
```

## Request Object Structure

Each request in `requestsList` has this structure:

```javascript
{
    id: "uuid-string",
    method: "GET",
    path: "/api/users",
    host: "example.com",
    url: "https://example.com/api/users",
    status: 200,
    duration: "45ms",           // Formatted string from server
    duration_ms: 45,            // Raw milliseconds for calculations
    timestamp: "2024-01-15T10:30:00Z",
    req_header: { "Content-Type": ["application/json"] },
    res_header: { "Content-Type": ["application/json"] },
    req_body: "...",
    res_body: "...",
    req_body_size: 1024,
    res_body_size: 2048,
    content_type: "application/json",
    tunnel_id: "tunnel-uuid"
}
```

## CSS Class Conventions

The base.js adds these classes you can style:

### Request Items

```css
.request-item { }           /* Each request in the list */
.request-item.selected { }  /* Currently selected request */
.request-item.pinned { }    /* Pinned requests */
```

### Status Classes

```css
.s-2xx { }  /* Success responses (200-299) */
.s-3xx { }  /* Redirects (300-399) */
.s-4xx { }  /* Client errors (400-499) */
.s-5xx { }  /* Server errors (500-599) */
```

### Method Classes

```css
.m-get { }
.m-post { }
.m-put { }
.m-patch { }
.m-delete { }
```

## Events

The base.js dispatches custom events you can listen for:

```javascript
// New request received
document.addEventListener('tunnel:request', (e) => {
    console.log('New request:', e.detail);
});

// Request selected
document.addEventListener('tunnel:select', (e) => {
    console.log('Selected:', e.detail.id);
});

// Requests cleared
document.addEventListener('tunnel:clear', () => {
    console.log('All requests cleared');
});
```

## Keyboard Shortcuts

These are built into base.js and work automatically:

| Key | Action |
|-----|--------|
| `j` / `↓` | Next request |
| `k` / `↑` | Previous request |
| `Enter` | View request details |
| `Escape` | Close detail view |
| `r` | Replay selected request |
| `p` | Pin/unpin request |
| `c` | Clear all requests |
| `/` | Focus search |
| `?` | Show keyboard shortcuts |

## Example: Minimal List Template

```html
<!DOCTYPE html>
<html>
<head>
    <title>Minimal Inspector</title>
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: ui-monospace, monospace;
            margin: 0;
            padding: 16px;
            background: #1a1a1a;
            color: #e0e0e0;
        }
        #request-list { max-width: 800px; margin: 0 auto; }
        .request-item {
            display: flex;
            gap: 12px;
            padding: 8px 12px;
            border-bottom: 1px solid #333;
            cursor: pointer;
        }
        .request-item:hover { background: #252525; }
        .request-item.selected { background: #2d4a3e; }
        .method { width: 60px; font-weight: bold; }
        .path { flex: 1; overflow: hidden; text-overflow: ellipsis; }
        .status { width: 40px; text-align: right; }
        .s-2xx { color: #4caf50; }
        .s-4xx { color: #ff9800; }
        .s-5xx { color: #f44336; }
        .duration { width: 70px; text-align: right; color: #888; }
        #empty-state { text-align: center; color: #666; padding: 40px; }
    </style>
</head>
<body>
    <div id="request-list"></div>
    <div id="empty-state">Waiting for requests...</div>
    {{BASE_JS}}
</body>
</html>
```

## Tips

1. **Start simple** - Begin with just `request-list` and add features incrementally
2. **Use CSS variables** - Makes theming easier
3. **Test with `--inspect-port`** - Use a different port to test alongside the default template
4. **Check the console** - The JS logs helpful debug info
5. **Reference the developer template** - See `client/templates/layouts/developer.html` for a full example