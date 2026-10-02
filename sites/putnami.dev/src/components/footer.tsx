import type { ReactNode } from 'react';
import { Link } from '@putnami/web';
import { styled } from '@putnami/ui';

// `release` is the island that shows the latest Putnami version (see
// app/release-version.island.tsx); the footer itself stays server-rendered.
export const Footer = ({ release }: { release?: ReactNode }) => (
  <StyledFooter as='footer'>
    <FooterLicense>
      {release}
      Licensed under{' '}
      <Link to='/LICENSE.md' target='_blank' rel='noopener noreferrer'>
        FSL-1.1-MIT
      </Link>
      {' — '}
      {/* The site measures its own audience without cookies. That is exempt
          from consent, never from telling people about it. */}
      <Link to='/privacy'>Privacy</Link>
    </FooterLicense>
  </StyledFooter>
);

const StyledFooter = styled.footer`
  text-align: center;
  padding: var(--space-2xl) var(--space-lg);
  margin-top: var(--space-2xl);
  border-top: 1px solid var(--color-border);
  color: var(--color-text-muted);
  font-size: 0.875rem;

  a {
    color: var(--color-text);
    text-decoration: none;
    transition: color var(--transition-fast);

    &:hover {
      color: var(--color-primary);
    }
  }
`;

const FooterLicense = styled.p`
  font-size: 0.8rem;
  color: var(--color-text-muted);
`;
