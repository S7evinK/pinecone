-- Copyright 2022 The Matrix.org Foundation C.I.C.
--
-- Licensed under the Apache License, Version 2.0 (the "License");
-- you may not use this file except in compliance with the License.
-- You may obtain a copy of the License at
--
--     http://www.apache.org/licenses/LICENSE-2.0
--
-- Unless required by applicable law or agreed to in writing, software
-- distributed under the License is distributed on an "AS IS" BASIS,
-- WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
-- See the License for the specific language governing permissions and
-- limitations under the License.
pinecone_protocol = Proto("pine", "Pinecone Protocol")

local frame_versions = {[0] = "Version 0"}

local frame_types = {
    [0] = "Keepalive",
    [1] = "Tree Announcement",
    [2] = "Tree Routed",
    [3] = "Bootstrap",
    [4] = "SNEK Routed",
    [5] = "SNEK Ping",
    [6] = "SNEK Pong",
    [7] = "Tree Ping",
    [8] = "Tree Pong"
}

header_size = 10
f_version_idx = 4
f_type_idx = 5
f_extra_idx = 6
f_len_idx = 8
f_payload_idx = header_size

magic_bytes = ProtoField.string("pinecone.magic", "Magic Bytes")
frame_version = ProtoField.uint8("pinecone.version", "Version", base.DEC,
                                 frame_versions)
frame_type = ProtoField.uint8("pinecone.type", "Type", base.DEC, frame_types)
extra_bytes = ProtoField.bytes("pinecone.extra", "Extra Bytes")
frame_len = ProtoField.uint16("pinecone.len", "Frame Length")

destination_len = ProtoField.uint16("pinecone.dstlen", "Destination Length")
source_len = ProtoField.uint16("pinecone.srclen", "Source Length")
payload_len = ProtoField.uint16("pinecone.payloadlen", "Payload Length")

destination = ProtoField.string("pinecone.dst", "Destination Coords")
destination_key = ProtoField.bytes("pinecone.dstkey", "Destination Key")
destination_sig = ProtoField.bytes("pinecone.dstsig", "Destination Signature")

source = ProtoField.string("pinecone.src", "Source Coords")
source_key = ProtoField.bytes("pinecone.srckey", "Source Key")
source_sig = ProtoField.bytes("pinecone.srcsig", "Source Signature")

hop_count = ProtoField.uint16("pinecone.hops", "Hop Count")
payload = ProtoField.bytes("pinecone.payload", "Payload", base.SPACE)

rootkey = ProtoField.bytes("pinecone.rootkey", "Root public key")
rootseq = ProtoField.uint32("pinecone.rootseq", "Root sequence number")
roottgt = ProtoField.bytes("pinecone.roottgt", "Provides coordinates")
sigport = ProtoField.uint8("pinecone.sigport", "Port")
sigkey = ProtoField.bytes("pinecone.sigkey", "Public key")
sigsig = ProtoField.bytes("pinecone.sigsig", "Signature")

bootstrap_seq = ProtoField.uint32("pinecone.bootstrapseq",
                                  "Bootstrap sequence number")

watermark_key = ProtoField.bytes("pinecone.wmarkkey", "Watermark public key")
watermark_seq = ProtoField.uint32("pinecone.wmarkseq",
                                  "Watermark sequence number")

pinecone_protocol.fields = {
    magic_bytes, frame_version, frame_type, extra_bytes, frame_len,
    destination_len, source_len, payload_len, destination, source,
    destination_key, source_key, destination_sig, source_sig, payload, rootkey,
    rootseq, sigkey, sigport, sigsig, roottgt, bootstrap_seq, watermark_key,
    watermark_seq
}

function short_pk(key)
    local h = Struct.tohex(key)
    return string.sub(h, 0, 4) .. "…" .. string.sub(h, 61, 64)
end

function full_pk(key) return Struct.tohex(key) end

function varu64(bytes)
    local n = 0
    local l = 0
    while l < 10 do
        local b = bytes:get_index(l)
        n = bit32.lshift(n, 7)
        n = bit32.bor(n, bit32.band(b, 0x7f))
        l = l + 1
        if bit32.band(b, 0x80) == 0 then break end
    end
    return n, l
end

function coords(bytes)
    local b = bytes:bytes()
    local c = {}
    local o = 0
    while o < b:len() do
        n, l = varu64(b:subset(o, b:len() - o))
        c[#c + 1] = n
        o = o + l
    end
    return "[" .. table.concat(c, " ") .. "]"
end

local function do_pinecone_length(buffer, pinfo, tree) return
    buffer(8, 2):uint() end

local function do_pinecone_dissect(buffer, pinfo, tree)
    local subtree = tree:add(pinecone_protocol, buffer(), "Pinecone Protocol")
    subtree:add_le(frame_version, buffer(f_version_idx, 1))
    subtree:add_le(frame_type, buffer(f_type_idx, 1))
    subtree:add_le(extra_bytes, buffer(f_extra_idx, 2))
    subtree:add_le(frame_len, buffer(f_len_idx, 2), buffer(f_len_idx, 2):uint())

    local ftype = buffer(5, 1):uint()
    if ftype == 0 then
        -- Keepalive
        pinfo.cols.info:set(frame_types[0])
    elseif ftype == 3 then
        -- Bootstrap
        local plen = buffer(f_payload_idx, 2):uint()
        local dstkey = buffer(f_payload_idx + 2, 32)

        local wmarkkey = buffer(f_payload_idx + 2 + 32, 32)
        subtree:add(watermark_key, buffer(f_payload_idx + 2 + 32, 32))
        local wmarkseq, offset = varu64(buffer(f_payload_idx + 2 + 64):bytes())
        subtree:add(watermark_seq, buffer(f_payload_idx + 2 + 64, offset),
                    wmarkseq)

        local pload = buffer(f_payload_idx + 2 + 32 + 32 + offset, plen)
        subtree:add(payload_len, buffer(f_payload_idx, 2), plen)
        subtree:add(destination_key, dstkey)

        local psubtree = subtree:add(subtree, pload, "Payload")
        psubtree:set_text("Payload")
        local seq, offset = varu64(pload(0):bytes())
        psubtree:add(bootstrap_seq, pload(0, offset), seq)
        psubtree:add(rootkey, pload(offset, 32))
        local root_seq, root_offset = varu64(pload(offset + 32):bytes())
        psubtree:add(rootseq, pload(offset + 32, root_offset), root_seq)
        psubtree:add(sigsig, pload(offset + 32 + root_offset, 64))

        -- Info column
        pinfo.cols.info:set(frame_types[3])
        pinfo.cols.info:append(" " .. short_pk(dstkey:bytes():raw()) .. " → ")
    elseif (ftype == 4 or ftype == 5 or ftype == 6) then
        -- SNEK Routed
        -- SNEK Ping
        -- SNEK Pong
        local plen = buffer(f_payload_idx, 2):uint()
        subtree:add(payload_len, buffer(f_payload_idx, 2), plen)
        local dstkey = buffer(f_payload_idx + 2, 32)
        subtree:add(destination_key, buffer(f_payload_idx + 2, 32))
        local srckey = buffer(f_payload_idx + 2 + 32, 32)
        subtree:add(source_key, buffer(f_payload_idx + 2 + 32, 32))

        local wmarkkey = buffer(f_payload_idx + 2 + 32 + 32, 32)
        subtree:add(watermark_key, buffer(f_payload_idx + 2 + 32 + 32, 32))
        local wmarkseq, offset = varu64(
                                     buffer(f_payload_idx + 2 + 64 + 32):bytes())
        subtree:add(watermark_seq, buffer(f_payload_idx + 2 + 64 + 32, offset),
                    wmarkseq)

        local pload = buffer(f_payload_idx + 2 + 64 + 32 + offset, plen)
        local psubtree = subtree:add(subtree, pload, "Payload")
        psubtree:set_text("Payload")

        if plen > 0 and ftype == 4 then
            -- SNEK Routed
            quic_dissector = Dissector.get("quic")
            quic_dissector:call(pload:tvb(), pinfo, tree)
            if pinfo.cols.protocol ~= pinecone_protocol.name then
                pinfo.cols.protocol:prepend(pinecone_protocol.name .. "-")
            end
            pinfo.cols.info:set(frame_types[8])
        end

        -- Info column
        pinfo.cols.info:append(" [" .. short_pk(srckey:string()) .. "] → [" ..
                                   short_pk(dstkey:string()) .. "]")
    else
        -- Tree Announcement
        -- Tree Routed
        -- Tree Ping
        -- Tree Pong
        local plen = buffer(f_payload_idx, 2):uint()
        subtree:add(payload_len, buffer(f_payload_idx, 2), plen)

        local dlen = buffer(f_payload_idx + 2, 2):uint()
        local slen = buffer(f_payload_idx + 4 + dlen, 2):uint()
        local dstcoords = coords(buffer(f_payload_idx + 2 + 2, dlen))
        local srccoords = coords(buffer(f_payload_idx + 4 + dlen + 2, slen))
        subtree:add(destination_len, buffer(f_payload_idx + 2, 2), dlen)
        subtree:add(destination, buffer(f_payload_idx + 4, dlen), dstcoords)
        subtree:add(source_len, buffer(f_payload_idx + 4 + dlen, 2), slen)
        subtree:add(source, buffer(f_payload_idx + 4 + dlen + 2, slen),
                    srccoords)

        local payload = buffer(f_payload_idx + 6 + dlen + slen, plen)

        if ftype == 1 then
            -- Tree Announcement
            local dhsubtree = subtree:add(subtree, payload, "Root Announcement")
            dhsubtree:add(rootkey, payload(0, 32))
            local seq, offset = varu64(payload(32):bytes())
            dhsubtree:add(rootseq, payload(0, offset), seq)
            pinfo.cols.info:append(" Seq=" .. seq)
            local tgt = dhsubtree:add(roottgt, payload, "None")
            offset = offset + 32
            local ports = {}
            while offset < payload:len() do
                local seq, o = varu64(payload(offset):bytes())
                local sigsubtree = dhsubtree:add(subtree, payload(offset, o),
                                                 "Ancestor Signature")
                sigsubtree:add(sigport, payload(offset, o), seq)
                sigsubtree:add(sigkey, payload(offset + o, 32))
                sigsubtree:add(sigsig, payload(offset + o + 32, 64))
                offset = offset + 32 + 64 + o
                sigsubtree:set_text("Ancestor Signature Coords=[" ..
                                        table.concat(ports, " ") .. "]")
                ports[#ports + 1] = seq
                tgt:set_text("Provides coordinates: [" ..
                                 table.concat(ports, " ") .. "]")
            end
            dhsubtree:set_text("Root Announcement (" .. #ports .. " signatures)")

            -- Info column
            pinfo.cols.info:set(frame_types[1])
            pinfo.cols.info:append(" Root=[" ..
                                       short_pk(payload(0, 32):bytes():raw()) ..
                                       "]")
            pinfo.cols.info:append(" Coords=[" .. table.concat(ports, " ") ..
                                       "]")
        elseif (ftype == 2 or ftype == 7 or ftype == 8) then
            if plen > 0 and ftype == 2 then
                -- Tree Routed
                quic_dissector = Dissector.get("quic")
                quic_dissector:call(payload:tvb(), pinfo, tree)
                if pinfo.cols.protocol ~= pinecone_protocol.name then
                    pinfo.cols.protocol:prepend(pinecone_protocol.name .. "-")
                end
                pinfo.cols.info:set(frame_types[2])
            end

            -- Info column
            pinfo.cols.info:append(srccoords .. " → " .. dstcoords)
        end
    end
end

function pinecone_protocol.dissector(buffer, pinfo, tree)
    length = buffer:len()
    if length < header_size then return end
    if buffer(0, 4):string() ~= "pine" then return end
    pinfo.cols.protocol:set(pinecone_protocol.name)

    dissect_tcp_pdus(buffer, tree, header_size, do_pinecone_length,
                     do_pinecone_dissect)
    return 1
end

pinecone_protocol:register_heuristic("tcp", pinecone_protocol.dissector)
