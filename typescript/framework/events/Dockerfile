ARG WORKSPACE_BUILDER_IMAGE=oven/bun:1.4-slim

FROM ${WORKSPACE_BUILDER_IMAGE}

ENV NODE_ENV=production
ENV PORT=4222
ENV PUTNAMI_OUTPUT=jsonl
EXPOSE 4222

CMD ["bunx", "putnami", "serve", "@putnami/events", "--watch=false"]
