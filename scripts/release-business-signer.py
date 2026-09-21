#!/usr/bin/env python3
"""Disposable HTTPS/OpenSSL signer using only CSRs and node public sealing keys."""
import base64
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import secrets
import ssl
import subprocess
import sys
import uuid

work = Path(sys.argv[1])
token = (work / 'private/certificate-signer-token').read_text().strip()
issued = {}
(work / 'signer-leaf.ext').write_text('basicConstraints=critical,CA:FALSE\n'
                                    'keyUsage=critical,digitalSignature,keyEncipherment\n'
                                    'extendedKeyUsage=clientAuth\n')


def openssl(*args, data=None):
    return subprocess.run(['openssl', *args], input=data, check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout


class Signer(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def reply(self, status, body=None):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        if body is not None:
            self.wfile.write(json.dumps(body).encode())

    def do_POST(self):
        if self.headers.get('Authorization') != 'Bearer ' + token:
            return self.reply(401)
        length = int(self.headers.get('Content-Length', '0'))
        if not 0 < length <= 65536:
            return self.reply(400)
        body = self.rfile.read(length)
        try:
            if self.path == '/sign/seal':
                if self.headers.get('X-Ocservia-Node-ID') != (work / 'signer-node').read_text().strip():
                    return self.reply(400)
                if self.headers.get('X-Ocservia-Seal-Purpose') != 'certificate_p12_password' or len(body) > 1024:
                    return self.reply(400)
                encrypted = openssl('pkeyutl', '-encrypt', '-pubin', '-inkey', str(work / 'p12.pub.pem'),
                                    '-pkeyopt', 'rsa_padding_mode:oaep', '-pkeyopt', 'rsa_oaep_md:sha256', data=body)
                return self.reply(200, {'sealed': base64.b64encode(encrypted).decode(), 'key_id': 't07-p12',
                                        'version': 1, 'purpose': 'certificate_p12_password'})
            request = json.loads(body)
            cert_id = str(uuid.UUID(request['certificate_id']))
            if self.path == '/sign':
                if self.headers.get('Idempotency-Key') != cert_id:
                    return self.reply(400)
                csr = base64.b64decode(request['csr_der'], validate=True)
                if cert_id in issued:
                    old_csr, chain, _revoked = issued[cert_id]
                    return self.reply(200, {'certificate_chain_pem': chain}) if old_csr == csr else self.reply(409)
                openssl('req', '-inform', 'DER', '-verify', '-noout', data=csr)
                cert = openssl('x509', '-req', '-inform', 'DER', '-copy_extensions', 'copy', '-days', '1', '-set_serial',
                               '0x' + secrets.token_hex(16), '-CA', str(work / 'ca.crt'),
                               '-CAkey', str(work / 'private/ca.key'), '-extfile', str(work / 'signer-leaf.ext'), data=csr).decode()
                chain = cert + (work / 'ca.crt').read_text()
                issued[cert_id] = (csr, chain, False)
                return self.reply(200, {'certificate_chain_pem': chain})
            if self.path == '/sign/revoke' and cert_id in issued:
                csr, chain, _ = issued[cert_id]
                serial = openssl('x509', '-noout', '-serial', data=chain.encode()).decode().strip().split('=')[1]
                if self.headers.get('Idempotency-Key') != cert_id + ':revoke' or int(request['serial_number']) != int(serial, 16):
                    return self.reply(400)
                issued[cert_id] = (csr, chain, True)
                (work / 'signer-revoked').write_text(cert_id)
                return self.reply(204)
            return self.reply(404)
        except (ValueError, KeyError, OSError, subprocess.CalledProcessError):
            return self.reply(400)


context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(work / 'secrets/tls.crt', work / 'private/tls.key')
server = HTTPServer((sys.argv[2], 19444), Signer)
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
