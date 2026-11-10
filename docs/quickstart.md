# Quickstart

Run ZAKS Drive on your machine in about five minutes, **without a Microsoft 365 tenant and without SAP**. It talks to a bundled Microsoft Graph emulator, so you can try the REST API and the SAP Content Server interface safely. Nothing here touches real SharePoint.

You need Docker with Compose v2, `git` and `curl`.

```bash
git clone https://github.com/Lamiv/zaks-drive.git zaks-drive
cd zaks-drive
docker compose -f loadtest/docker-compose.yml up -d --build graphmock adapter
```

The stack uses `deploy/config.mock.yaml`: API key `loadtest-key`, repository `DMS`, SAP content repository `Z1` with signatures switched off. These settings are for the emulator only.

## Try the REST API

```bash
BASE=http://localhost:8080/api/v1
KEY=loadtest-key

curl -sS -H "X-API-Key: $KEY" "$BASE/repositories"

echo "hello ZAKS Drive" > hello.txt
curl -sS -X POST -H "X-API-Key: $KEY" --data-binary @hello.txt \
  "$BASE/repositories/DMS/documents?folder=demo&fileName=hello.txt"

curl -sS -H "X-API-Key: $KEY" "$BASE/repositories/DMS/children?path=demo"
```

Copy the `id` from the upload answer and read the file back with `GET $BASE/repositories/DMS/documents/<id>/content`. More calls: [REST API guide](api/rest-api.md).

## Try the SAP Content Server interface

```bash
curl -sS 'http://localhost:8090/ContentServer/ContentServer.dll?serverInfo&pVersion=0047'
curl -sS -X PUT --data-binary 'first original' -H 'Content-Type: text/plain' \
  'http://localhost:8090/ContentServer/ContentServer.dll?create&pVersion=0047&contRep=Z1&docId=0123456789ABCDEF&compId=data'
curl -sS 'http://localhost:8090/ContentServer/ContentServer.dll?get&pVersion=0047&contRep=Z1&docId=0123456789ABCDEF&compId=data'
```

Real SAP systems sign these URLs; see [Connect SAP S/4HANA](sap/connect-s4hana-to-sharepoint.md).

## Look around

```bash
curl -sS localhost:9090/readyz                     # ready
curl -sS localhost:9090/metrics | grep ^adapter_   # Prometheus metrics
docker compose -f loadtest/docker-compose.yml logs adapter | tail
```

## Stop

```bash
docker compose -f loadtest/docker-compose.yml down -v
```

## Next

- Set up a real server and follow the scenarios: [setup and usage guide](guide.md); or just the install steps: [installation](installation.md).
- Connect SAP: [Connect SAP S/4HANA to SharePoint using ZAKS Drive](sap/connect-s4hana-to-sharepoint.md).
- Understand the design: [architecture](architecture.md).
