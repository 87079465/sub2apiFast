import json
import subprocess
import time
import webbrowser
import requests

CLIENT_ID = "d3590ed6-52b3-4102-aeff-aad2292ab01c"
SCOPE = "https://substrate.office.com/sydney/v2/.default offline_access openid profile"
PROXIES = {"http": "http://127.0.0.1:7897", "https": "http://127.0.0.1:7897"}

def main():
    resp = requests.post(
        "https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode",
        data={"client_id": CLIENT_ID, "scope": SCOPE},
        proxies=PROXIES,
        timeout=15
    )
    resp.raise_for_status()
    data = resp.json()
    user_code = data["user_code"]
    device_code = data["device_code"]
    uri = data.get("verification_uri", "https://microsoft.com/devicelogin")
    interval = int(data.get("interval", 5))

    print(f"==========================================")
    print(f"Verification URI: {uri}")
    print(f"User Code:        {user_code}")
    print(f"==========================================", flush=True)

    try:
        webbrowser.open(uri)
    except Exception:
        pass

    token_url = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
    while True:
        time.sleep(interval)
        r = requests.post(
            token_url,
            data={
                "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                "client_id": CLIENT_ID,
                "device_code": device_code,
            },
            proxies=PROXIES,
            timeout=15
        )
        res = r.json()
        if r.status_code == 200 and "access_token" in res:
            print("[+] Successfully obtained Access Token & Refresh Token!", flush=True)
            creds = {
                "access_token": res["access_token"],
                "refresh_token": res.get("refresh_token", ""),
                "client_id": CLIENT_ID,
                "expires_in": res.get("expires_in", 3600),
                "expires_at": int(time.time()) + int(res.get("expires_in", 3600)),
                "scenario": "ExcelSidepaneWebPremiumConsumerCopilot"
            }
            with open("tools/office_oauth_token.json", "w", encoding="utf-8") as f:
                json.dump(creds, f, indent=2)

            creds_json = json.dumps(creds).replace("'", "''")
            sql = f"UPDATE accounts SET credentials = '{creds_json}'::jsonb, status = 'active', updated_at = NOW() WHERE platform = 'office_copilot';"
            with open("C:/Windows/Temp/update_copilot_token.sql", "w", encoding="utf-8") as f:
                f.write(sql)

            subprocess.run([
                "C:/pgsql/17/pgsql/bin/psql.exe",
                "-U", "sub2api", "-h", "127.0.0.1", "-d", "sub2api",
                "-f", "C:/Windows/Temp/update_copilot_token.sql"
            ], env={"PGPASSWORD": "sub2api", "SystemRoot": "C:\Windows", "PATH": "C:\pgsql\17\pgsql\bin"})
            print("[+] Database account updated successfully!", flush=True)
            break
        elif res.get("error") == "authorization_pending":
            continue
        else:
            print("[-] Error:", res, flush=True)
            break

if __name__ == "__main__":
    main()
