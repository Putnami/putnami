package com.example.calc;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Disabled;
import org.junit.jupiter.api.Nested;
import org.junit.jupiter.api.Test;

class CalcTest {
    @Test
    void add() {
        assertEquals(5, Calc.add(2, 3));
    }

    @Nested
    class Sign {
        @Test
        void nonNegativeInput() {
            assertEquals("positive", Calc.sign(4));
        }
    }

    @Test
    void addWrongOnPurpose() {
        assertEquals(5, Calc.add(2, 2), "two plus two should be five");
    }

    @Test
    @Disabled("not ready")
    void skipped() {
    }
}
