import type { ReactNode } from 'react';
import { isRouteErrorResponse, useRouteError } from 'react-router';
import { ErrorBoundary } from './error-boundary';

export function NotFoundErrorBoundary({
  notFoundElement,
  fallbackErrorElement,
}: {
  notFoundElement: ReactNode;
  fallbackErrorElement?: ReactNode;
}) {
  const error = useRouteError();
  if (isRouteErrorResponse(error) && error.status === 404) {
    return <>{notFoundElement}</>;
  }
  return <>{fallbackErrorElement ?? <ErrorBoundary />}</>;
}
