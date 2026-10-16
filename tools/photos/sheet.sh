sheet () {
	out=$1 
	shift
	n=$# 
	ins=() 
	f="" 
	i=0 
	for x in "$@"
	do
		ins+=(-i "$x") 
		f+="[$i]scale=300:400:force_original_aspect_ratio=decrease,pad=300:400:(ow-iw)/2:(oh-ih)/2[v$i];" 
		i=$((i+1)) 
	done
	st="" 
	for ((j=0; j<n; j++)) do
		st+="[v$j]" 
	done
	ffmpeg -v error -y "${ins[@]}" -filter_complex "${f}${st}hstack=$n" "$out"
}
