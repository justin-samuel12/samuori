#!/bin/sh
# Creates the rehearsal's certificates and Secrets in namespace samuori. Run inside the samuori-k3s VM.
set -eu
d=$(mktemp -d)
cd "$d"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj /CN=rehearsal-nats-ca -keyout ca.key -out ca.crt 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=nats -keyout server.key -out server.csr 2>/dev/null
printf 'subjectAltName=DNS:nats,DNS:*.nats,DNS:*.nats.samuori.svc,DNS:*.nats.samuori.svc.cluster.local\nextendedKeyUsage=serverAuth,clientAuth\n' >server.ext
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 7 -extfile server.ext -out server.crt 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=samuori-controller -keyout client.key -out client.csr 2>/dev/null
printf 'extendedKeyUsage=clientAuth\n' >client.ext
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 7 -extfile client.ext -out client.crt 2>/dev/null
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj /CN=localhost -addext subjectAltName=DNS:localhost,IP:127.0.0.1 -keyout api.key -out api.crt 2>/dev/null
pw=$(openssl rand -hex 16)
head -c 32 /dev/urandom >cluster.key
k() { kubectl -n samuori "$@"; }
kubectl create namespace samuori >/dev/null 2>&1 || true
k create secret generic rehearsal-nats-server --from-file=tls.crt=server.crt --from-file=tls.key=server.key --from-file=ca.crt=ca.crt
k create secret generic rehearsal-nats-ca --from-file=ca.crt=ca.crt
k create secret tls rehearsal-nats-client --cert=client.crt --key=client.key
k create secret tls rehearsal-api-tls --cert=api.crt --key=api.key
k create secret generic rehearsal-postgres --from-literal=password="$pw"
k create secret generic rehearsal-database --from-literal=dsn="postgres://samuori:$pw@postgres:5432/samuori?sslmode=disable"
k create secret generic rehearsal-cluster-key --from-file=cluster.key=cluster.key
age_pub=${SAMUORI_REHEARSAL_BACKUP_RECIPIENT:?set the age public key that backups are encrypted to}
k create secret generic rehearsal-backup --from-literal=recipients.txt="$age_pub"
cp ca.crt "$HOME/rehearsal-nats-ca.crt"
rm -rf "$d"
