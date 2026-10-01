"""String manipulation utilities."""


def reverse(s: str) -> str:
    """Reverse a string."""
    return s[::-1]


def capitalize(s: str) -> str:
    """Capitalize the first letter of a string."""
    if not s:
        return s
    return s[0].upper() + s[1:]


def is_palindrome(s: str) -> bool:
    """Check if a string is a palindrome (case-insensitive, ignoring spaces)."""
    normalized = s.lower().replace(" ", "")
    return normalized == normalized[::-1]
