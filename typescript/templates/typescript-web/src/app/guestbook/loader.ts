import { loader } from '@putnami/web';

export interface GuestbookEntry {
  id: number;
  name: string;
  message: string;
  createdAt: string;
}

// In-memory store for demo purposes
const entries: GuestbookEntry[] = [
  { id: 1, name: 'Putnami', message: 'Welcome to the guestbook!', createdAt: new Date().toISOString() },
];

let nextId = 2;

export function addEntry(name: string, message: string): GuestbookEntry {
  const entry: GuestbookEntry = { id: nextId++, name, message, createdAt: new Date().toISOString() };
  entries.unshift(entry);
  return entry;
}

export function getEntries(): GuestbookEntry[] {
  return [...entries];
}

export default loader(() => ({ entries: getEntries() }));
