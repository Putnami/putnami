import { describe, expect, it } from 'bun:test';
import { getMimeFromPath } from '../../src/static/mime-types';

describe('getMimeFromPath', () => {
  it('returns correct mime for common extensions', () => {
    expect(getMimeFromPath('index.html')).toBe('text/html');
    expect(getMimeFromPath('style.css')).toBe('text/css');
    expect(getMimeFromPath('app.js')).toBe('application/javascript');
    expect(getMimeFromPath('data.json')).toBe('application/json');
    expect(getMimeFromPath('image.png')).toBe('image/png');
    expect(getMimeFromPath('photo.jpg')).toBe('image/jpeg');
    expect(getMimeFromPath('photo.jpeg')).toBe('image/jpeg');
    expect(getMimeFromPath('icon.svg')).toBe('image/svg+xml');
    expect(getMimeFromPath('doc.pdf')).toBe('application/pdf');
    expect(getMimeFromPath('font.woff2')).toBe('font/woff2');
    expect(getMimeFromPath('font.woff')).toBe('font/woff');
    expect(getMimeFromPath('font.ttf')).toBe('font/ttf');
  });

  it('returns correct mime for paths with directories', () => {
    expect(getMimeFromPath('public/assets/style.css')).toBe('text/css');
    expect(getMimeFromPath('/static/image.png')).toBe('image/png');
  });

  it('returns octet-stream for unknown extensions', () => {
    expect(getMimeFromPath('file.xyz')).toBe('application/octet-stream');
    expect(getMimeFromPath('file.unknown')).toBe('application/octet-stream');
  });

  it('returns octet-stream for files without extension', () => {
    expect(getMimeFromPath('Makefile')).toBe('application/octet-stream');
  });

  it('handles media file extensions', () => {
    expect(getMimeFromPath('video.mp4')).toBe('video/mp4');
    expect(getMimeFromPath('audio.mp3')).toBe('audio/mpeg');
    expect(getMimeFromPath('video.webm')).toBe('video/webm');
    expect(getMimeFromPath('image.webp')).toBe('image/webp');
    expect(getMimeFromPath('image.avif')).toBe('image/avif');
    expect(getMimeFromPath('image.gif')).toBe('image/gif');
  });

  it('handles archive extensions', () => {
    expect(getMimeFromPath('archive.zip')).toBe('application/zip');
    expect(getMimeFromPath('archive.gz')).toBe('application/gzip');
    expect(getMimeFromPath('archive.7z')).toBe('application/x-7z-compressed');
    expect(getMimeFromPath('archive.tar')).toBe('application/x-tar');
  });

  it('handles text extensions', () => {
    expect(getMimeFromPath('readme.txt')).toBe('text/plain');
    expect(getMimeFromPath('readme.md')).toBe('text/markdown');
    expect(getMimeFromPath('data.csv')).toBe('text/csv');
    expect(getMimeFromPath('data.xml')).toBe('application/xml');
  });

  it('uses last dot for extension', () => {
    expect(getMimeFromPath('file.backup.json')).toBe('application/json');
    expect(getMimeFromPath('archive.tar.gz')).toBe('application/gzip');
  });
});
