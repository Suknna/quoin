import http from "node:http";

// Local-only real-mode prerequisite for the service-worker transition test.
http.createServer((request, response) => {
	if (request.url === "/api/v1/auth/me") {
		response.writeHead(200, { "Content-Type": "application/json" });
		response.end(JSON.stringify({ id: "stub-admin", username: "stub", displayName: "Stub Admin", role: "admin", enabled: true, passwordChangeRequired: false, authRevision: 1, rowVersion: 1, lastLoginAt: null }));
		return;
	}
	if (request.url === "/api/v1/maintenance") {
		response.writeHead(200, { "Content-Type": "application/json" });
		response.end(JSON.stringify({ active: false, rowVersion: 1, items: [] }));
		return;
	}
	response.writeHead(404, { "Content-Type": "application/json" });
	response.end(JSON.stringify({ message: "local real-mode stub has no route" }));
}).listen(4188, "127.0.0.1");
