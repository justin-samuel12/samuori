const express = require("express");

const app = express();
const orders = [{ id: 1, item: "coffee", quantity: 2 }];

app.get("/", (_req, res) => res.json({ service: "orders-api", status: "ok" }));
app.get("/orders", (_req, res) => res.json(orders));

const port = Number(process.env.PORT ?? 3000);
app.listen(port, () => console.log(`orders-api listening on ${port}`));
