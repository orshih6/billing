# Kubernetes example

A starting point, not a chart. Adjust the host, image tag and Ingress class.

```bash
kubectl create namespace billing
kubectl -n billing create secret generic billing-secrets \
  --from-literal=DATABASE_URL='postgres://billing:<password>@<host>:5432/billing?sslmode=require' \
  --from-literal=SECRETS_KEY="$(openssl rand -hex 32)" \
  --from-literal=UI_COOKIE_KEY="$(openssl rand -hex 32)"

kubectl apply -f migrate-job.yaml
kubectl -n billing wait --for=condition=complete job/billing-migrate --timeout=120s
kubectl apply -f billing.yaml
```

**Back up `SECRETS_KEY` with your database password.** It encrypts tenants'
payment-provider credentials; without it they cannot be read.

Production refuses to start without a platform key. Mint one (it is printed
once):

```bash
kubectl -n billing run billing-keys --rm -it --restart=Never --image=ghcr.io/orshih6/billing:v0.1.0 \
  --env="DATABASE_URL=$(kubectl -n billing get secret billing-secrets -o jsonpath='{.data.DATABASE_URL}' | base64 -d)" \
  -- keys create --platform --name platform-admin --scopes admin
```

- **Worker**: the engine and webhook dispatcher are safe on several replicas
  (rows are claimed under lock), but one is plenty to start.
- **TLS**: terminate at your Ingress. Set `PUBLIC_URL` to the HTTPS address, so
  UI cookies are marked `Secure`.
