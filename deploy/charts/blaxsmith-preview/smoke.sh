#!/bin/sh
set -eu

api_name="blaxsmith-preview-api-$$"
web_name="blaxsmith-preview-web-$$"
trap 'docker rm -f "$web_name" "$api_name" >/dev/null 2>&1 || true' EXIT

docker build --target api -t blaxsmith-preview-api:smoke .
docker build --target web -t blaxsmith-preview-web:smoke .
docker run -d --name "$api_name" --read-only --cap-drop ALL \
  --security-opt no-new-privileges blaxsmith-preview-api:smoke serve --port 8001 >/dev/null
docker run -d --name "$web_name" --network "container:$api_name" \
  --read-only --tmpfs /tmp:rw,uid=65532,gid=65532,mode=1777 \
  --cap-drop ALL --security-opt no-new-privileges blaxsmith-preview-web:smoke >/dev/null

attempt=0
until docker exec "$web_name" sh -ec 'nc -z 127.0.0.1 8001 && wget -q -O /dev/null http://127.0.0.1:8080/tools/'; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 20 ]; then
    docker logs "$api_name" >&2
    docker logs "$web_name" >&2
    exit 1
  fi
  sleep 1
done
docker exec "$web_name" sh -ec \
  'wget -S -O /dev/null http://127.0.0.1:8080/api/blaxsmith.api.v1.CatalogService/ListTools 2>&1 | grep -q "405 Method Not Allowed"'
echo 'restricted preview images and internal API proxy passed'
