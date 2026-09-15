// Isolated OIDC provider for production login, not a Controller/auth mock.
// One password-protected test principal; short-lived one-use PKCE codes.
import https from "node:https";
import fs from "node:fs";
import crypto from "node:crypto";
const directory = process.argv[2];
const issuer = process.argv[3];
const secret = fs.readFileSync(`${directory}/oidc-client-secret`, "utf8").trim();
const { privateKey, publicKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
const jwk = { ...publicKey.export({ format: "jwk" }), kid: "upgrade", use: "sig", alg: "RS256" };
const codes = new Map();
const encode = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");
https.createServer({ key: fs.readFileSync(`${directory}/tls.key`), cert: fs.readFileSync(`${directory}/tls.crt`) }, async (req, res) => {
  const url = new URL(req.url, issuer);
  const json = (status, data) => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify(data)); };
  if (url.pathname === "/.well-known/openid-configuration") return json(200, {
    issuer, authorization_endpoint: `${issuer}/authorize`, token_endpoint: `${issuer}/token`, jwks_uri: `${issuer}/keys`,
    response_types_supported: ["code"], subject_types_supported: ["public"], id_token_signing_alg_values_supported: ["RS256"],
    code_challenge_methods_supported: ["S256"], token_endpoint_auth_methods_supported: ["client_secret_basic", "client_secret_post"],
  });
  if (url.pathname === "/keys") return json(200, { keys: [jwk] });
  if (url.pathname === "/authorize") {
    if (req.headers.authorization !== `Basic ${Buffer.from(`upgrade:${secret}`).toString("base64")}`) {
      res.writeHead(401, { "WWW-Authenticate": 'Basic realm="upgrade fixture"' }); return res.end();
    }
    const p = url.searchParams;
    if (p.get("client_id") !== "upgrade" || p.get("response_type") !== "code" || p.get("code_challenge_method") !== "S256" ||
      p.get("redirect_uri") !== "https://localhost/api/v1/auth/callback" || !p.get("nonce")) return json(400, { error: "invalid_request" });
    const code = crypto.randomBytes(32).toString("hex");
    codes.set(code, { nonce: p.get("nonce"), challenge: p.get("code_challenge"), expires: Date.now() + 60000 });
    const target = new URL(p.get("redirect_uri"));
    target.searchParams.set("state", p.get("state")); target.searchParams.set("code", code);
    res.writeHead(302, { Location: target.href }); return res.end();
  }
  if (url.pathname === "/token" && req.method === "POST") {
    let body = "";
    for await (const chunk of req) { body += chunk; if (body.length > 8192) return json(413, {}); }
    const p = new URLSearchParams(body);
    const basic = req.headers.authorization === `Basic ${Buffer.from(`upgrade:${secret}`).toString("base64")}`;
    if (!basic && !(p.get("client_id") === "upgrade" && p.get("client_secret") === secret)) return json(401, { error: "invalid_client" });
    const code = codes.get(p.get("code")); codes.delete(p.get("code"));
    if (!code || code.expires < Date.now() || p.get("grant_type") !== "authorization_code" ||
      p.get("redirect_uri") !== "https://localhost/api/v1/auth/callback" ||
      crypto.createHash("sha256").update(p.get("code_verifier") || "").digest("base64url") !== code.challenge) return json(400, { error: "invalid_grant" });
    const now = Math.floor(Date.now() / 1000);
    const payload = `${encode({ alg: "RS256", kid: "upgrade" })}.${encode({ iss: issuer, aud: "upgrade", sub: "upgrade-operator", nonce: code.nonce,
      iat: now, exp: now + 120, email: "upgrade@example.invalid", name: "Upgrade Operator" })}`;
    return json(200, { token_type: "Bearer", access_token: crypto.randomBytes(32).toString("hex"), expires_in: 120,
      id_token: `${payload}.${crypto.sign("RSA-SHA256", Buffer.from(payload), privateKey).toString("base64url")}` });
  }
  return json(404, {});
}).listen(19443, "0.0.0.0");
