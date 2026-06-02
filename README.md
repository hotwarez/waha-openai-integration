# WAHA + OpenAI Assistants Bridge

## Visão geral
Este repositório contém um **bridge** escrito em Go que conecta a **WAHA (WhatsApp HTTP API)** a um **Assistente da OpenAI**. A WAHA já está implantada em seu ambiente; este serviço recebe os webhooks da WAHA, envia a mensagem para o assistente da OpenAI e devolve a resposta ao usuário via WAHA.

## Como funciona
1. **WAHA → webhook**: a WAHA envia um `POST /webhook` para este serviço contendo `chatId` (número do WhatsApp) e `text`.
2. **Bridge**: cria (ou reutiliza) um *thread* na OpenAI, adiciona a mensagem do usuário, inicia um *run* e aguarda a resposta.
3. **Resposta → WAHA**: o texto retornado pela OpenAI é enviado de volta ao usuário usando o endpoint `POST /api/sendText` da WAHA.

## Variáveis de ambiente
| Variável | Descrição | Exemplo |
|---|---|---|
| `WAHA_URL` | URL base da sua instância WAHA (ex.: `http://waha:8000`). | `http://waha:8000` |
| `WAHA_TOKEN` | Token de autenticação da WAHA (se necessário). | `mytoken` |
| `OPENAI_API_KEY` | Sua chave de API da OpenAI. | `sk-...` |
| `OPENAI_ASSISTANT_ID` | ID do assistente criado na OpenAI. | `asst_12345` |
| `USE_RESPONSES_API` | `true` para usar a nova Responses API (ainda em fase beta). | `false` |
| `PORT` (opcional) | Porta onde a aplicação escuta (padrão `8080`). | `8080` |

## Executando localmente (Docker)
```bash
# Clone o repositório (já está no seu GitHub)
git clone https://github.com/hotwarez/waha-openai-integration.git
cd waha-openai-integration

# Crie o .env (ou exporte as variáveis)
cat > .env <<EOF
WAHA_URL=http://<IP_OU_HOST_DA_WAHA>:8000
WAHA_TOKEN=
OPENAI_API_KEY=seu_key_openai
OPENAI_ASSISTANT_ID=seu_assistant_id
USE_RESPONSES_API=false
EOF

# Build e run
docker build -t hotwarez/waha-openai:latest .

docker run -d \
  --name waha-openai \
  -p 8080:8080 \
  --env-file .env \
  hotwarez/waha-openai:latest
```
A aplicação ficará disponível em `http://<host>:8080`. Verifique a saúde:
```bash
curl http://localhost:8080/health
# => {"status":"ok"}
```

## Docker‑Compose (stack) – somente o bridge
Como a WAHA já está rodando, basta levantar este serviço:
```yaml
version: "3.8"
services:
  bridge:
    build: .
    image: docker.io/hotwarez/waha-openai:latest
    ports:
      - "8080:8080"   # porta interna da aplicação
    environment:
      - WAHA_URL=http://waha:8000   # ajuste se necessário
      - WAHA_TOKEN=
      - OPENAI_API_KEY=${OPENAI_API_KEY}
      - OPENAI_ASSISTANT_ID=${OPENAI_ASSISTANT_ID}
      - USE_RESPONSES_API=false
    restart: unless-stopped
```
Coloque esse arquivo como `docker-compose.yml` ao lado do código e rode:
```bash
docker compose up -d
```
Certifique‑se de que a variável `WHATSAPP_HOOK_URL` da sua instância WAHA aponte para `http://<IP_DO_BRIDGE>:8080/webhook`.

## Persistência de threads
Os IDs dos threads da OpenAI são armazenados em `./threads/<chatId>.txt`. Para que esses arquivos sobrevivam a reinicializações, adicione um volume:
```yaml
    volumes:
      - ./threads:/app/threads
```

## CI/CD – GitHub Actions (build e push da imagem)
Para automatizar o build da imagem Docker e publicar no Docker Hub, adicione o workflow abaixo em `.github/workflows/docker.yml`:
```yaml
name: Build & Push Docker Image

on:
  push:
    branches: [ main ]
  pull_request:
    branches: [ main ]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout repository
        uses: actions/checkout@v3

      - name: Set up QEMU
        uses: docker/setup-qemu-action@v2

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v2

      - name: Log in to Docker Hub
        uses: docker/login-action@v2
        with:
          username: ${{ secrets.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - name: Build and push
        uses: docker/build-push-action@v4
        with:
          context: .
          push: true
          tags: docker.io/hotwarez/waha-openai:latest
```
> **Importante:** Crie os segredos `DOCKERHUB_USERNAME` e `DOCKERHUB_TOKEN` nas *Settings → Secrets* do seu repositório.

## Teste rápido
1. Garanta que o webhook da WAHA está configurado para `http://<IP_BRIDGE>:8080/webhook`.
2. Envie uma mensagem para o número WhatsApp configurado.
3. O bot deve responder com a resposta do assistente da OpenAI.

Caso encontre algum problema (erro 400, timeout, etc.), verifique os logs do container:
```bash
docker logs waha-openai
```
Eles contêm a mensagem de erro da OpenAI ou da WAHA.

---
**Pronto!** Seu bridge está preparado para ser implantado ao lado da WAHA e servir como camada de inteligência artificial via OpenAI Assistants.
