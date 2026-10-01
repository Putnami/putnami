import { createIcon, type Icon } from './icon';

export * from './icon';

export const ClockIcon: Icon = createIcon('clock', [
  ['path', { d: 'M12 6v6l4 2' }],
  ['circle', { cx: 12, cy: 12, r: 10 }],
]);

export const DropletsIcon: Icon = createIcon('droplets', [
  [
    'path',
    {
      d: 'M7 16.3c2.2 0 4-1.83 4-4.05 0-1.16-.57-2.26-1.71-3.19S7.29 6.75 7 5.3c-.29 1.45-1.14 2.84-2.29 3.76S3 11.1 3 12.25c0 2.22 1.8 4.05 4 4.05z',
    },
  ],
  [
    'path',
    {
      d: 'M12.56 6.6A10.97 10.97 0 0 0 14 3.02c.5 2.5 2 4.9 4 6.5s3 3.5 3 5.5a6.98 6.98 0 0 1-11.91 4.97',
    },
  ],
]);

export const LogOutIcon: Icon = createIcon('log-out', [
  ['path', { d: 'm16 17 5-5-5-5' }],
  ['path', { d: 'M21 12H9' }],
  ['path', { d: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4' }],
]);

export const ShieldIcon: Icon = createIcon('shield', [
  [
    'path',
    {
      d: 'M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z',
    },
  ],
]);

export const TargetIcon: Icon = createIcon('target', [
  ['circle', { cx: 12, cy: 12, r: 10 }],
  ['circle', { cx: 12, cy: 12, r: 6 }],
  ['circle', { cx: 12, cy: 12, r: 2 }],
]);

export const TrendingUpIcon: Icon = createIcon('trending-up', [
  ['path', { d: 'M16 7h6v6' }],
  ['path', { d: 'm22 7-8.5 8.5-5-5L2 17' }],
]);

export const WalletIcon: Icon = createIcon('wallet', [
  [
    'path',
    {
      d: 'M19 7V4a1 1 0 0 0-1-1H5a2 2 0 0 0 0 4h15a1 1 0 0 1 1 1v4h-3a2 2 0 0 0 0 4h3a1 1 0 0 0 1-1v-2a1 1 0 0 0-1-1',
    },
  ],
  ['path', { d: 'M3 5v14a2 2 0 0 0 2 2h15a1 1 0 0 0 1-1v-4' }],
]);

export const ZapIcon: Icon = createIcon('zap', [
  [
    'path',
    {
      d: 'M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z',
    },
  ],
]);
