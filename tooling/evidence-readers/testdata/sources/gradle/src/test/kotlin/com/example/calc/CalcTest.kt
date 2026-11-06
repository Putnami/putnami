package com.example.calc

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Disabled
import org.junit.jupiter.api.Nested
import org.junit.jupiter.api.Test

class CalcTest {
    @Test
    fun add() {
        assertEquals(5, Calc.add(2, 3))
    }

    @Nested
    inner class Sign {
        @Test
        fun nonNegativeInput() {
            assertEquals("positive", Calc.sign(4))
        }
    }

    @Test
    fun addWrongOnPurpose() {
        assertEquals(5, Calc.add(2, 2), "two plus two should be five")
    }

    @Test
    @Disabled("not ready")
    fun skipped() {
    }
}
