'use client';

import { SearchModal, useDocSearch } from './search-modal';

/**
 * Client-side search provider.
 * Renders the search modal and listens for Cmd+K / Ctrl+K.
 */
export function SearchProvider() {
  const { isOpen, close } = useDocSearch();

  return <SearchModal isOpen={isOpen} onClose={close} />;
}
