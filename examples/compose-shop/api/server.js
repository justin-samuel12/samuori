const http = require("node:http");

http
  .createServer((req, res) => {
    res.writeHead(200, { "content-type": "text/plain" });
    res.end(req.url === "/healthz" ? "ok" : "shop api");
  })
  .listen(3000);
