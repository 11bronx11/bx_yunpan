#!/usr/bin/env python3
"""Add missing AI split identities without printing or replacing existing secrets."""
import base64
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys

path = Path(sys.argv[1])
content = path.read_text()
values = dict(re.findall(r'^([A-Z][A-Z0-9_]*)=(.*)$', content, re.M))
updates = {}
for key in ('AI_POSTGRES_PASSWORD','AI_QUERY_PASSWORD','AI_COMPUTE_PASSWORD','AI_COMMIT_PASSWORD','CORE_INTERNAL_SERVICE_TOKEN','AI_QUERY_SERVICE_TOKEN','PARSER_SERVICE_TOKEN'):
    if not values.get(key):
        updates[key] = secrets.token_hex(32)
for prefix in ('AI_SCOPE','AI_PROCESSING'):
    if values.get(prefix+'_SIGNING_SEED'):
        continue
    if values.get(prefix+'_PUBLIC_KEY'):
        raise SystemExit(prefix+' public key exists without its signing seed; restore the original seed')
    private = subprocess.check_output(['openssl','genpkey','-algorithm','ED25519','-outform','DER'], stderr=subprocess.DEVNULL)
    public = subprocess.check_output(['openssl','pkey','-inform','DER','-pubout','-outform','DER'], input=private, stderr=subprocess.DEVNULL)
    encode = lambda value: base64.urlsafe_b64encode(value[-32:]).decode().rstrip('=')
    updates[prefix+'_SIGNING_SEED'] = encode(private)
    updates[prefix+'_PUBLIC_KEY'] = encode(public)
for key,value in updates.items():
    pattern = r'^'+re.escape(key)+r'=.*$'
    if re.search(pattern,content,re.M):
        content = re.sub(pattern,key+'='+value,content,flags=re.M)
    else:
        content += '\n'+key+'='+value+'\n'
if updates:
    path.write_text(content)
    os.chmod(path,0o600)
print('AI service identities initialized; existing values preserved.')
