from pylib import capitalize, is_palindrome, reverse


def test_reverse_empty():
    assert reverse("") == ""


def test_reverse_single():
    assert reverse("a") == "a"


def test_reverse_word():
    assert reverse("hello") == "olleh"


def test_reverse_unicode():
    assert reverse("abc") == "cba"


def test_reverse_with_spaces():
    assert reverse("hello world") == "dlrow olleh"


def test_capitalize_empty():
    assert capitalize("") == ""


def test_capitalize_single():
    assert capitalize("a") == "A"


def test_capitalize_word():
    assert capitalize("hello") == "Hello"


def test_capitalize_already():
    assert capitalize("Hello") == "Hello"


def test_capitalize_with_spaces():
    assert capitalize("hello world") == "Hello world"


def test_palindrome_empty():
    assert is_palindrome("") is True


def test_palindrome_single():
    assert is_palindrome("a") is True


def test_palindrome_true():
    assert is_palindrome("racecar") is True


def test_palindrome_with_spaces():
    assert is_palindrome("a man a plan a canal panama") is True


def test_palindrome_false():
    assert is_palindrome("hello") is False


def test_palindrome_case_insensitive():
    assert is_palindrome("Racecar") is True
