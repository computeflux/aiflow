#!/usr/bin/env python3
"""Test X (Twitter) OAuth 1.0a consumer key/secret against api.x.com."""
import hashlib
import hmac
import base64
import time
import secrets
import urllib.parse
import urllib.request
import urllib.error


def pct(s: str) -> str:
    return urllib.parse.quote(s, safe='')


KEY = 'T3BTV2g4enl4R2VEVm5ZY2RvMVg6MTpjaQ'
SECRET = 'ngSp-PptuFAj-I_Zp8apKh-b6kvcCi91-vN7gKymE2IigILXNw'
CALLBACK = 'http://xiaobai.asyou.me:4007/integrations/social/x'
URL = 'https://api.x.com/oauth/request_token'

nonce = secrets.token_hex(16)
ts = str(int(time.time()))

params = {
    'oauth_callback': CALLBACK,
    'oauth_consumer_key': KEY,
    'oauth_nonce': nonce,
    'oauth_signature_method': 'HMAC-SHA1',
    'oauth_timestamp': ts,
    'oauth_version': '1.0',
}

param_str = '&'.join(f'{pct(k)}={pct(params[k])}' for k in sorted(params))
base = '&'.join(['POST', pct(URL), pct(param_str)])
sig_key = pct(SECRET) + '&'
sig = base64.b64encode(
    hmac.new(sig_key.encode(), base.encode(), hashlib.sha1).digest()
).decode()
params['oauth_signature'] = sig
auth = 'OAuth ' + ', '.join(
    f'{pct(k)}="{pct(params[k])}"' for k in sorted(params)
)

req = urllib.request.Request(
    URL, data=b'', method='POST', headers={'Authorization': auth}
)
try:
    resp = urllib.request.urlopen(req, timeout=20)
    print('STATUS:', resp.status)
    print(resp.read().decode())
except urllib.error.HTTPError as e:
    print('HTTP ERROR:', e.code)
    print(e.read().decode())
except Exception as e:
    print('OTHER ERROR:', repr(e))
