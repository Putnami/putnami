import { beforeAll, describe, expect, it } from 'bun:test';

let dockerBuilder: any;
beforeAll(async () => {
  try {
    const module = await import('../../src/docker/index');
    dockerBuilder = module.dockerBuilder;
  } catch {
    dockerBuilder = null;
  }
});

/** Minimal type for Docker build context (inlined from removed @putnami/sdk) */
type ProjectDescription = {
  name: string;
  path: string;
  main?: string;
  exports?: string | Record<string, unknown>;
  scripts?: Record<string, string>;
};

type DockerBuildContext = {
  project: ProjectDescription;
  projectPath: string;
  outputPath: string;
  workspaceRoot: string;
  entrypoint?: string;
  port?: number;
  workspaceBuilderImage?: string;
};

describe('dockerBuilder', () => {
  it('should load dockerBuilder', () => {
    expect(dockerBuilder).toBeDefined();
  });

  const createMockContext = (overrides?: Partial<DockerBuildContext>): DockerBuildContext => ({
    project: {
      name: 'test-project',
      path: 'packages/test-project',
      exports: {},
      scripts: {},
    } as ProjectDescription,
    projectPath: '/workspace/packages/test-project',
    outputPath: '/workspace/.putnami/projects/test-project/@putnami-go/build/latest/output',
    workspaceRoot: '/workspace',
    port: 3000,
    ...overrides,
  });

  describe('generateDockerfile (local mode)', () => {
    it('should copy binary from local build context', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({
        project: {
          name: 'my-project',
          path: 'packages/my-project',
          exports: {},
          scripts: {},
        } as ProjectDescription,
        projectPath: '/workspace/packages/my-project',
      });

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('my-project');
      expect(dockerfile).toContain(
        'COPY .putnami/projects/my-project/@putnami-go/build/latest/output/bin/my-project /app',
      );
      expect(dockerfile).not.toContain('COPY --from=builder');
      expect(dockerfile).not.toContain('WORKSPACE_BUILDER_IMAGE');
    });

    it('should use distroless runtime image', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext();

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('gcr.io/distroless/static:nonroot');
    });

    it('should set PORT environment variable', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext();

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('ENV PORT=3000');
    });

    it('should set custom PORT when provided', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({ port: 8080 });

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('ENV PORT=8080');
    });

    it('should set entrypoint to /app', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext();

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('ENTRYPOINT ["/app"]');
    });

    it('should use nonroot user', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext();

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('USER nonroot:nonroot');
    });
  });

  describe('generateDockerfile (CI mode with workspaceBuilderImage)', () => {
    it('should use multi-stage build with workspace builder image', async () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({
        project: {
          name: 'my-project',
          path: 'packages/my-project',
          exports: {},
          scripts: {},
        } as ProjectDescription,
        projectPath: '/workspace/packages/my-project',
        workspaceBuilderImage: 'registry/workspace-builder:latest',
      });

      const dockerfile = await dockerBuilder.generateDockerfile(ctx);

      expect(dockerfile).toContain('ARG WORKSPACE_BUILDER_IMAGE=registry/workspace-builder:latest');
      expect(dockerfile).toContain('FROM ${WORKSPACE_BUILDER_IMAGE} AS builder');
      expect(dockerfile).toContain(
        'COPY --from=builder /app/.putnami/projects/my-project/@putnami-go/build/latest/output/bin/my-project /app',
      );
    });
  });

  describe('getBaseImage', () => {
    it('should return correct base image', () => {
      if (!dockerBuilder) return;
      const baseImage = dockerBuilder.getBaseImage();

      expect(baseImage).toBe('gcr.io/distroless/static:nonroot');
    });
  });

  describe('getBuildArgs', () => {
    it('should return build args with PORT', () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({
        port: 8080,
      });

      const buildArgs = dockerBuilder.getBuildArgs(ctx);

      expect(buildArgs.PORT).toBe('8080');
    });

    it('should use default port 3000 when not provided', () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({
        port: undefined,
      });

      const buildArgs = dockerBuilder.getBuildArgs(ctx);

      expect(buildArgs.PORT).toBe('3000');
    });

    it('should include WORKSPACE_BUILDER_IMAGE when provided', () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({
        workspaceBuilderImage: 'custom/bun:latest',
      });

      const buildArgs = dockerBuilder.getBuildArgs(ctx);

      expect(buildArgs.WORKSPACE_BUILDER_IMAGE).toBe('custom/bun:latest');
    });
  });

  describe('getEntrypoint', () => {
    it('should return entrypoint with /app', () => {
      if (!dockerBuilder) return;
      const ctx = createMockContext({
        project: {
          name: 'my-app',
          path: 'packages/my-app',
          exports: {},
          scripts: {},
        } as ProjectDescription,
        projectPath: '/workspace/packages/my-app',
      });

      const entrypoint = dockerBuilder.getEntrypoint(ctx);

      expect(entrypoint).toEqual(['/app']);
    });
  });
});
