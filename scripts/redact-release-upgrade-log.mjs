import fs from "node:fs";
const [source, secretDirectory, output] = process.argv.slice(2);
let log = fs.readFileSync(source, "utf8");
const secrets = [];
for (const name of fs.readdirSync(secretDirectory)) {
  const value = fs.readFileSync(`${secretDirectory}/${name}`, "utf8").trim();
  if (value.length >= 8) secrets.push(value);
  for (const line of value.split("\n")) if (line.length >= 16) secrets.push(line);
}
for (const secret of secrets.sort((a, b) => b.length - a.length)) log = log.split(secret).join("[REDACTED]");
log = log.replace(/(postgres(?:ql)?:\/\/)[^\s@]+@/gi, "$1[REDACTED]@");
fs.writeFileSync(output, log, { mode: 0o600 });
