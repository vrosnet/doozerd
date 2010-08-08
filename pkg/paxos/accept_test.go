package paxos

import (
    "fmt"
    "strings"
    "strconv"

    "borg/assert"
    "testing"
)

const (
    iSender = iota
    iCmd
    iRnd
    iNumParts
)

func accept(quorum int, ins, outs chan string) {
    var rnd, vrnd uint64
    var vval string

    ch, sent := make(chan int), 0
    for in := range ins {
        parts := strings.Split(in, ":", 3)
        if len(parts) != iNumParts {
            continue
        }
        switch parts[iCmd] {
        case "INVITE":
            i, _ := strconv.Btoui64(parts[iRnd], 10)
            // If parts[iRnd] is invalid, i is 0 and the message will be ignored
            switch {
                case i <= rnd:
                case i > rnd:
                    rnd = i

                    sent++
                    msg := fmt.Sprintf("ACCEPT:%d:%d:%s", i, vrnd, vval)
                    go func(msg string) { outs <- msg ; ch <- 1 }(msg)
            }
        }
    }

    for x := 0; x < sent; x++ {
        <-ch
    }

    close(outs)
}



// TESTING

func slurp(ch chan string) (got string) {
    for x := range ch { got += x }
    return
}

func TestAcceptsInvite(t *testing.T) {
    ins := make(chan string)
    outs := make(chan string)

    exp := "ACCEPT:1:0:"

    go accept(2, ins, outs)
    // Send a message with no senderId
    ins <- "1:INVITE:1"
    close(ins)

    // outs was closed; therefore all messages have been processed
    assert.Equal(t, exp, slurp(outs), "")
}

func TestIgnoresStaleInvites(t *testing.T) {
    ins := make(chan string)
    outs := make(chan string)

    exp := "ACCEPT:2:0:"

    go accept(2, ins, outs)
    // Send a message with no senderId
    ins <- "1:INVITE:2"
    ins <- "1:INVITE:1"
    close(ins)

    // outs was closed; therefore all messages have been processed
    assert.Equal(t, exp, slurp(outs), "")
}

func TestIgnoresMalformedMessages(t *testing.T) {
    totest := []string{
        "x", // too few separators
        "x:x", // too few separators
        "x:x:x:x", // too many separators
        "1:INVITE:x", // invalid round number
        "1:x:1", // unknown command
    }
    for _, msg := range(totest) {
        ins := make(chan string)
        outs := make(chan string)

        exp := ""

        go accept(2, ins, outs)
        // Send a message with no senderId
        ins <- msg
        close(ins)

        // outs was closed; therefore all messages have been processed
        assert.Equal(t, exp, slurp(outs), "")
    }
}
